// Package secretresolver is a thin client for an external service that turns
// opaque secret references into secret values at deploy time.
//
// The point of the indirection is that Portainer stores the reference and never
// the value: the compose body keeps a plain ${VAR:?message} and the stack's Env
// field holds a reference, so nothing that reads Portainer's database or its API
// ever sees a live credential. The values are fetched into memory for the
// duration of one deploy and injected through libstack.Options.Env, which never
// touches disk.
//
// The package deliberately depends on nothing from Portainer. It also does not
// parse the reference beyond its prefix: the scheme after "secret:" is routed by
// the resolver, so adding a secret backend is a resolver config change and never
// a Portainer rebuild.
//
// # The one contract every resolver implementation must honour
//
// The "error" field of a response is the only resolver-supplied text Portainer ever
// surfaces to its API and its database. Everything else a resolver sends is either a
// secret value - which stays in memory for the duration of one deploy - or is
// discarded without being quoted: a body that fails to parse, a body over the size
// cap, the raw body behind a non-2xx status and a round trip that fails before any
// response can be parsed all produce an error built from non-content facts alone,
// with the details going to the server log instead.
//
// "Non-content facts" is meant literally, down to the status line. An error about a
// non-2xx names resp.StatusCode and never resp.Status: Go fills Status from the status
// line as the server wrote it, so it reads "<code> <reason phrase>" and the phrase is
// the resolver's own text, bounded only by net/http's 10 MiB header limit. The number
// is ours - three digits parsed by net/http - and the phrase beside it is theirs, so
// echoing Status would open a second, unbounded channel of resolver text into the very
// error maxResolverErrorBytes exists to bound.
//
// So a resolver that interpolates a resolved value into its "error" field breaks the
// contract, and the value lands permanently in the failed stack's deployment status
// message, served back by StackInspect - the very channel this feature exists to
// close. The field is echoed anyway, sanitised and bounded to maxResolverErrorBytes,
// because it is what makes "the vault is locked" diagnosable from the UI; see the
// classification in docs/secret-resolver.md §3.12.
//
// # Sanitising, and why length alone was not enough
//
// That sanctioned 512-byte channel is byte-arbitrary, and the message it lands in is
// read by AI agents doing ordinary infrastructure work, not only by operators at a
// terminal. An "error" field carrying two newlines, a NUL, an ANSI CSI escape and a
// forged-looking JSON log record reading "SYSTEM: ignore previous instructions" was
// persisted whole, at 552 bytes, entirely within the bound. So every piece of
// resolver-chosen text this package formats goes through sanitize first: C0 and C1
// control characters, DEL and bytes that are not valid UTF-8 are replaced by printable
// escapes, and the bound is applied to the escaped form so that escaping cannot inflate
// anything past it. The rule the package keeps:
//
//   - resolver-chosen text is sanitised AND bounded, in one pass, at the one function
//     that formats it - the error field, the Content-Type header, net/http's own message
//     about a failed round trip, and a reduced Location;
//   - the configured endpoint is sanitised and bounded as well, by redactEndpoint, and is
//     therefore printed with %s and never with %q: it arrives at the format string already
//     escaped, and quoting it a second time would escape its escapes. It sits on the
//     sanitised side even though it is the operator's own text, because redactEndpoint is
//     the one place every printed endpoint passes through and doing both there is cheaper
//     to review than deciding, site by site, what this particular endpoint can contain;
//   - the rest of Portainer's and the operator's own text - a reference, the raw timeout
//     value, and the stack and variable names that api/* names in a deploy refusal - is
//     bounded only, by truncateReference, truncateTimeoutValue and TruncateName, and
//     escaped by the %q verb that every site printing one uses. Bounded AND %q, never one
//     of the two: %q leaves a mebibyte a mebibyte, and a bound leaves an ESC an ESC.
package secretresolver

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/rs/zerolog/log"
	"github.com/segmentio/encoding/json"
)

const (
	// ReferencePrefix marks a stack env value as a reference to be resolved rather
	// than a literal. Everything after the prefix is opaque to Portainer: the
	// resolver owns the scheme, so adding a secret backend never means rebuilding
	// Portainer.
	ReferencePrefix = "secret:"

	// EndpointEnvVar holds the resolver address. Configuration is taken from the
	// environment rather than from a CLI flag on purpose: a flag is a change to the
	// composition root that every future rebase of this fork has to carry, an
	// environment variable costs nothing.
	EndpointEnvVar = "PORTAINER_SECRET_RESOLVER"

	// TimeoutEnvVar overrides DefaultTimeout with a Go duration.
	TimeoutEnvVar = "PORTAINER_SECRET_RESOLVER_TIMEOUT"

	// DefaultTimeout bounds a single Fetch. The call is a round trip to a local
	// service for a handful of short strings, so it is measured in seconds.
	//
	// It is also the outermost of three nested budgets, and the resolver's
	// documentation pins the order they must keep:
	//
	//	RBW_TIMEOUT_SECONDS(20) < REQUEST_TIMEOUT_SECONDS(45) < this(60)
	//
	// The innermost bounds one vault subprocess, the middle one bounds a whole
	// resolve request, and this one bounds the round trip. Each must outlive the one
	// inside it, or the inner timeout can never fire and its diagnostic - the one
	// that says which call hung - is never printed. Lowering this number alone
	// silently disables the resolver's own error reporting, so move the chain
	// together.
	//
	// The middle and outer numbers were 25 and 30 and moved together to 45 and 60, because
	// a resolve request is not one subprocess call but 1+N of them: a sync followed by one
	// "rbw get" per reference. A worst-case sync of 20 s plus 11.8 s of process-spawn
	// overhead at MAX_REFS=256 - 257 spawns at about 46 ms each, measured on an idle
	// eight-core box - is 31.8 s, which fits 45 and never fitted 25. There is nothing to
	// add on top for network latency: "rbw get" reads a local cache and never contacts the
	// server. At 25 the middle budget could only fire in the first seconds of a request
	// that was going to take half a minute.
	DefaultTimeout = 60 * time.Second
)

const (
	// resolvePath is the resolver's only endpoint.
	resolvePath = "/v1/resolve"

	// protocolVersion lets the resolver evolve its wire format without a change on
	// the Portainer side.
	protocolVersion = 1

	// maxResponseBytes caps the response read so a broken resolver cannot balloon
	// the memory of the Portainer server.
	//
	// Enforced twice, and only one of the two is visible in the returned error: the
	// io.LimitReader in Fetch decides how much is READ, and the length check beside it turns
	// the result into a message that says which of "oversized" and "corrupt" it was. A test
	// that asserts on the message passes with the limit reader deleted - which is the state
	// this package was in, with io.ReadAll free to pull an unbounded body into memory. What
	// the reader does is only visible from the far end, so
	// TestFetch/"stops reading at the cap rather than merely reporting it" measures how far
	// a resolver streaming 32 MiB gets before the client hangs up.
	maxResponseBytes = 1 << 20 // 1 MiB

	// maxResolverErrorBytes bounds the resolver's own error field, the only external
	// text this client ever puts into an error of its own. That error is persisted as
	// the stack's deployment status message and served back over the API, so its size
	// is Portainer's problem rather than the resolver's: a resolver that answers with a
	// stack trace or a page of vault output must not be able to write it into the
	// database. Bounding it is not a secrecy control - see the package doc for why the
	// field is trusted to be value-free - it is a control over how much of somebody
	// else's text Portainer stores and serves.
	//
	// Size is only half of it. The field is byte-arbitrary within that bound, and the
	// message it lands in is read by agents as well as by operators, so it is sanitised
	// before it is bounded; see sanitize.
	maxResolverErrorBytes = 512

	// maxEndpointBytes bounds the endpoint as configured - and url.Parse's own reason for
	// refusing one - wherever either reaches an error or a log line. It is the operator's
	// text rather than the resolver's, which is why it was treated as free; it is bounded
	// because of where it goes, not because of who wrote it. Measured with a mebibyte in
	// PORTAINER_SECRET_RESOLVER, before and after:
	//
	//	transport error, unix endpoint       1048698 -> 364 bytes
	//	configErr, unparsable port           1048735 -> 390 bytes
	//	configErr, endpoint with no host     1048621 -> 296 bytes
	//	configErr, unsupported scheme        1048682 -> 359 bytes
	//
	// Three of those four are configErr, which EVERY Fetch returns and Portainer persists as
	// the stack's deployment status message - so one mistyped environment variable published
	// a mebibyte per deploy of every stack that uses references, rather than once.
	//
	// 256 bytes is a socket path, a host and a port with room to spare.
	maxEndpointBytes = 256

	// maxTimeoutValueBytes bounds the raw PORTAINER_SECRET_RESOLVER_TIMEOUT value before it
	// is quoted back at the operator. Same channel as maxEndpointBytes in kind - the
	// operator's own configuration, kept as configErr and therefore returned by every
	// Fetch - and a separate number for the reason every bound in this block is separate:
	// retuning one must not silently retune another. A Go duration that needs more than
	// this is not a Go duration.
	maxTimeoutValueBytes = 64

	// maxReferenceBytes bounds a reference named in an error. A reference is Portainer's
	// own data rather than the resolver's, but it is a stack env value, so its length is
	// whatever whoever edited the stack put there - and the error naming it is persisted
	// as that stack's deployment status message.
	maxReferenceBytes = 256

	// maxNameBytes bounds a stack's name, or the name of one of its variables, before
	// api/* quotes it into a deploy error. See TruncateName for the channel and for why
	// the bound is not the only half of the fix.
	//
	// The same number as maxReferenceBytes, because it is the same kind of text out of the
	// same request body, and a separate constant for the reason every bound in this block
	// is separate: retuning one must not silently retune another. A compose variable name
	// or a docker project name that needs more than this is neither.
	maxNameBytes = 256

	// maxContentTypeBytes bounds the resolver's Content-Type header before it is logged.
	// It is a separate number from maxResolverErrorBytes even though the two happen to
	// agree: they bound different channels - the error field goes to the database, the
	// header only to the log - and retuning one must not silently retune the other. A
	// content type that needs more than this is not a content type.
	maxContentTypeBytes = 512

	// maxTransportDetailBytes bounds net/http's own message about a failed round trip
	// before it is logged. Separate from the two bounds above for the same reason they are
	// separate from each other: it bounds a third channel and retuning one must not
	// silently retune the others.
	//
	// It exists because net/http builds its response-parse errors by quoting the header
	// text the resolver wrote, verbatim and in full: a bad Content-Length, a duplicated
	// one, an unsupported Transfer-Encoding and a malformed status line are all reported
	// as `<what> "<the offending header>"`, bounded by nothing below
	// MaxResponseHeaderBytes at 10 MiB. Measured with the header planted at a mebibyte,
	// the returned error was 1048724 bytes and carried the planted text whole. None of
	// that may reach the returned error, which Portainer persists as the stack's
	// deployment status message; a bounded copy in the log is the same trade
	// maxContentTypeBytes already makes, and it is what keeps a genuinely broken resolver
	// diagnosable at all.
	maxTransportDetailBytes = 512

	// maxRedirectTargetBytes bounds the reduced Location of a refused redirect before it
	// is logged - see logRefusedRedirect. Reducing the Location to scheme, host and port
	// already drops the room a resolver has to write free text into it, but a host name is
	// resolver-written text too and nothing below the 10 MiB header limit bounds it.
	//
	// The host position is the one that matters and it is the one a test has to drive:
	// redactLocation drops the path BEFORE this bound is applied, so a mebibyte planted in
	// the path is gone either way and a probe built on one is falsely green. Measured with
	// the mebibyte in the HOST instead and this bound removed, the log line was 1048872
	// bytes. TestFetchLogsARefusedRedirectTarget/"a hostile host" is the pin.
	maxRedirectTargetBytes = 256

	// truncationMarker is appended to text cut short at one of the bounds above, so a
	// truncated message is not read as a complete one.
	truncationMarker = "..."
)

// escapePrefix is the escaped form of ReferencePrefix. A value that begins with it is a
// literal whose own text starts with "secret:", not a reference - see Unescape.
const escapePrefix = ReferencePrefix + ":"

// IsReference reports whether a stack env value is a reference to be resolved
// rather than a literal value.
//
// A value beginning with the escaped marker "secret::" is a literal, not a reference.
func IsReference(value string) bool {
	return strings.HasPrefix(value, ReferencePrefix) && !strings.HasPrefix(value, escapePrefix)
}

// Unescape turns an escaped marker back into the literal it stands for. The marker's
// colon is doubled to escape it, so "secret::x" is the literal "secret:x" and
// "secret:::x" is the literal "secret::x". Every other value is returned unchanged.
//
// The escape exists because the marker is a bare prefix and there is otherwise no way
// out of it. A stack variable whose value legitimately begins with "secret:" - some
// third-party application's own URI-ish configuration format - would be undeployable on
// this fork with no workaround available in Portainer's UI: the compose path would fail
// on a resolver that cannot make sense of the reference, and the swarm and unpacker
// paths refuse a reference outright. Doubling one colon costs a character and keeps
// detection unambiguous, which is the whole reason a fixed marker was chosen.
func Unescape(value string) string {
	if !strings.HasPrefix(value, escapePrefix) {
		return value
	}

	return ReferencePrefix + strings.TrimPrefix(value, escapePrefix)
}

// LogEscapedValues records, once per deploy, that a stack carried values which only look
// like references and were unescaped on the way to the container.
//
// Every other consequence of the marker is loud: a reference with no resolver configured
// fails the deploy, a reference on the swarm or unpacker path is refused by name. This one
// sub-case is silent - an existing stack whose value happens to begin with "secret::"
// deploys successfully on this fork with a value one colon shorter than vanilla Portainer
// gives it - so it gets a line of its own. Nothing else would tell an operator that the
// value the container received is not the value the stack stores.
//
// The count and not the values: a value that only looks like a reference is still an
// operator's configuration, and the point of the line is that an escape happened at all.
func LogEscapedValues(stackName string, count int) {
	if count == 0 {
		return
	}

	log.Info().
		Str("stack", stackName).
		Int("variables", count).
		Msg("Stack values that only look like secret references were unescaped, they reach the stack one colon shorter than they are stored")
}

// Client fetches secret values from the resolver service.
type Client struct {
	httpClient *http.Client
	url        string
	timeout    time.Duration

	// endpoint is the address as configured, reduced by redactEndpoint, and it is the one
	// piece of variable text the errors of a failed round trip are allowed to carry: it is
	// the operator's own configuration rather than anything the resolver wrote, it is
	// bounded by whatever they put in the environment variable, and without it "the secret
	// resolver could not be reached" does not say which resolver. It is kept separately
	// from url because url is a request URL with resolvePath appended, which is a
	// different question from "what was this client pointed at".
	endpoint string

	// configErr carries a broken environment configuration. The composition root
	// builds the client from a constructor whose signature cannot return an error,
	// and dropping the error there would degrade a misconfiguration into "no
	// resolver configured" - which is precisely the silent fallback that would let a
	// stack deploy with an unresolved reference. So the error is kept and every
	// Fetch fails with it, loudly and at the point where it matters.
	configErr error
}

// FromEnv builds a client from the environment. It returns nil when
// PORTAINER_SECRET_RESOLVER is unset, meaning no resolver is configured.
func FromEnv() *Client {
	endpoint := os.Getenv(EndpointEnvVar)
	if endpoint == "" {
		return nil
	}

	timeout := DefaultTimeout
	if raw := os.Getenv(TimeoutEnvVar); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			// Bounded, and the parser's own error dropped rather than wrapped: it says
			// `time: invalid duration "<the whole value>"`, quoting the value a second
			// time and bounding it no better than %q did. This error becomes configErr
			// and is therefore returned by every Fetch and persisted as the deployment
			// status message of every stack that uses references, so a mebibyte in the
			// environment variable published a mebibyte per deploy. The expected form is
			// named instead, which is the whole of what the dropped text said.
			return misconfigured(fmt.Errorf("invalid %s value %q, expected a Go duration such as 60s", TimeoutEnvVar, truncateTimeoutValue(raw)))
		}

		timeout = parsed
	}

	client, err := New(endpoint, timeout)
	if err != nil {
		return misconfigured(err)
	}

	// Logged so a typo in the endpoint surfaces at startup rather than at the first
	// deploy of a migrated stack - but never the endpoint as given. Userinfo in an
	// http(s) endpoint is refused by New above, so a credential can only reach this line
	// through the unix:// form, where it is a stretch of the socket path rather than a
	// credential - and the redaction holds for both without having to tell them apart.
	safeEndpoint := redactEndpoint(endpoint)

	log.Info().Str("endpoint", safeEndpoint).Dur("timeout", timeout).Msg("Secret resolver configured")

	if isPlaintextRemote(endpoint) {
		log.Warn().Str("endpoint", safeEndpoint).Msg("Secret resolver endpoint is plain HTTP to a non-loopback host, resolved secret values will cross the network in clear text")
	}

	return client
}

// stripUserinfo returns rawURL without its userinfo, which is the only part of a URL that can
// hold a credential, and reports whether it parsed at all. The whole userinfo goes, not
// url.Redacted()'s masked password: Redacted keeps the username, and a token passed as
// "http://<token>@resolver:9100" lives there.
//
// This is the one place that rule lives. No URL of any kind is formatted into an error or a
// log line in this package without passing through here, which is cheaper to review than
// re-deciding, site by site, whether this particular message can carry userinfo.
//
// The rule and nothing else: it applies no bound. redactEndpoint and redactLocation bound
// different channels - the operator's own configuration and a resolver-written Location - and
// a bound applied here would be whichever of the two is smaller, silently, with the other
// one left dead. That is not hypothetical: this function was redactEndpoint, redactLocation
// went through it, and one bound of 256 bytes therefore made maxRedirectTargetBytes
// unreachable. Its test went green with the truncation deleted, which is precisely the shape
// of defect this package has now hit three times.
func stripUserinfo(rawURL string) (string, bool) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", false
	}

	parsed.User = nil

	return parsed.String(), true
}

// redactEndpoint returns the endpoint without its userinfo, sanitised and bounded to
// maxEndpointBytes. Everything a diagnosis needs - scheme, host, port, socket path - is kept.
//
// The bound is here rather than at the call sites because the endpoint is the operator's own
// text and was therefore treated as free for four rounds: a mebibyte in
// PORTAINER_SECRET_RESOLVER produced a 1048698-byte transport error, returned by Fetch and
// persisted as the stack's deployment status message. Sanitising is belt and braces on top:
// url.Parse already refuses a URL containing an ASCII control byte, and URL.String
// percent-encodes what is left, so nothing should reach sanitize with anything to escape -
// "should" being exactly the word that makes it worth the one call.
func redactEndpoint(endpoint string) string {
	stripped, ok := stripUserinfo(endpoint)
	if !ok {
		// Unparsable, so nothing in it can be classified as safe to print.
		return "(unparsable endpoint)"
	}

	return sanitize(stripped, maxEndpointBytes)
}

// redactLocation reduces a resolver-chosen Location header to scheme, host and port, which
// is everything the diagnosis in logRefusedRedirect needs and the most that may be said.
//
// It is stripUserinfo's rule - the userinfo goes - plus the removal of the path, the query
// and the fragment, and the difference between this and redactEndpoint is the difference
// between the two inputs. An endpoint is the operator's own configuration and its path is
// load bearing: for a unix:// endpoint the path IS the socket. A Location is text the
// resolver wrote, its path and query are free-text fields bounded only by net/http's 10 MiB
// header limit, and a host name is bounded by nothing below that either - hence the bound the
// caller applies on top.
//
// It goes through stripUserinfo and NOT through redactEndpoint, which is a distinction worth
// the words: the shared rule is the userinfo, not the bound. Routing this through
// redactEndpoint applied maxEndpointBytes here as well, which is smaller than nothing but is
// also not this channel's number - and it made maxRedirectTargetBytes unreachable, so deleting
// that bound left the package green. Whatever bounds a Location has to be a bound on
// Locations.
func redactLocation(location string) string {
	if location == "" {
		// A 3xx is not obliged to carry a Location and 300 and 304 routinely do not, so
		// this is an ordinary shape rather than a hostile one. It needs its own answer
		// because url.Parse("") succeeds, with an empty scheme and an empty host, and
		// would therefore be reported below as "(relative location)" - an assertion about
		// a header that was not there at all.
		return "(no location)"
	}

	parsed, err := url.Parse(location)
	if err != nil {
		// Unparsable, so nothing in it can be classified as safe to print.
		//
		// This is also the one shape the diagnostic used to lose altogether; see
		// redirectReporter for why it is reported from the transport rather than from
		// the response Fetch is handed.
		return "(unparsable location)"
	}

	if parsed.Scheme == "" && parsed.Host == "" {
		// A relative Location means "the same host I am already being asked at", so its
		// only content is the path - which is the part that must not be logged.
		return "(relative location)"
	}

	stripped, ok := stripUserinfo((&url.URL{Scheme: parsed.Scheme, Host: parsed.Host}).String())
	if !ok {
		return "(unparsable location)"
	}

	// Unbounded on purpose: sanitizeRedirectTarget is the caller's, and it is the only bound
	// on this path so that deleting it is visible.
	return stripped
}

// isPlaintextRemote reports whether the endpoint sends the resolver's answers - which
// are the secret values themselves - unencrypted over a network rather than over a
// loopback interface or a unix socket.
func isPlaintextRemote(endpoint string) bool {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "http" {
		return false
	}

	host := parsed.Hostname()
	if host == "localhost" {
		return false
	}

	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return false
	}

	return true
}

// misconfigured returns a client that fails every Fetch with the given
// configuration error. See Client.configErr for why this is not a nil client.
func misconfigured(err error) *Client {
	log.Error().Err(err).Msg("Invalid secret resolver configuration, deploying a stack that uses secret references will fail")

	return &Client{configErr: err}
}

// New returns a client for the given resolver endpoint. Two forms are supported:
// "unix:///path/to/resolver.sock" and "http://host:port" (or "https://").
func New(endpoint string, timeout time.Duration) (*Client, error) {
	if endpoint == "" {
		return nil, errors.New("secret resolver endpoint is empty")
	}

	if timeout <= 0 {
		return nil, fmt.Errorf("secret resolver timeout must be positive, got %s", timeout)
	}

	switch {
	case strings.HasPrefix(endpoint, "unix://"):
		// No userinfo refusal here, unlike the http(s) branch below: the dialer below receives
		// everything after "unix://" as a filesystem path, so a "user:token@" in it is part of
		// that path and reaches nobody as a credential - there is no remote to authenticate to.
		//
		// Not because the endpoint goes unparsed. It does get parsed, by the redactEndpoint
		// calls in this branch, and that parse has a consequence worth knowing: measured,
		// "unix://user:token@/run/x.sock" dials the path "user:token@/run/x.sock" while every
		// message and log line naming this client says "unix:///run/x.sock". The printed form
		// is therefore not always the path in use. That is the userinfo rule doing its job on a
		// string that only looks like it has userinfo, and it costs a slice of the real path in
		// diagnostics - which is the safe direction, and the reason this is stated rather than
		// special-cased.
		socketPath := strings.TrimPrefix(endpoint, "unix://")
		if socketPath == "" {
			// %s and not %q at this and every other endpoint-printing site: redactEndpoint
			// sanitises what it returns, and %q on top would escape its escapes.
			return nil, fmt.Errorf("secret resolver endpoint %s has no socket path", redactEndpoint(endpoint))
		}

		var dialer net.Dialer

		return &Client{
			httpClient: &http.Client{
				// See refuseRedirects. A redirect is never followed on either endpoint form.
				CheckRedirect: refuseRedirects,
				// See redirectReporter. Wrapped on both endpoint forms, for the same reason
				// CheckRedirect is set on both.
				Transport: &redirectReporter{base: &http.Transport{
					DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
						return dialer.DialContext(ctx, "unix", socketPath)
					},
				}},
			},
			// The host is ignored by the dialer above but net/http still requires a
			// syntactically valid URL.
			url: "http://localhost" + resolvePath,
			// Which says "unix:///run/secret-resolver.sock" rather than the useless
			// "http://localhost" above, and is what a failed round trip names.
			endpoint: redactEndpoint(endpoint),
			timeout:  timeout,
		}, nil

	case strings.HasPrefix(endpoint, "http://"), strings.HasPrefix(endpoint, "https://"):
		// A prefix check alone accepts "http://", whose trimmed form is the nonsense
		// "http:" - and the mistake would then only surface on the first deploy of a
		// migrated stack, as "no Host in request URL". Reject it here instead.
		parsed, err := url.Parse(endpoint)
		if err != nil {
			// url.Parse fails with a *url.Error, and a *url.Error prints the URL it was
			// handed in full: the password masking that hides a credential in net/http's
			// errors is applied by net/http, not by the error type. Wrapping this one would
			// put the endpoint's userinfo into configErr, which every Fetch then returns and
			// Portainer persists as the stack's deployment status message. So only the inner
			// reason is kept and the endpoint is not named.
			//
			// The endpoint is not named; its text is not entirely withheld, and this comment
			// said otherwise for four rounds. url.Parse builds its inner errors by quoting the
			// fragment that failed - `invalid port ":<the port>" after host`, `invalid URL
			// escape "%zz"` - so a slice of the endpoint rides out inside the reason, and
			// nothing bounded it: measured with a mebibyte in the port position, configErr came
			// back at 1048735 bytes, on every Fetch of every stack that uses references. No
			// credential reaches it, verified over a 38-input corpus rather than reasoned
			// about - url.Parse splits the userinfo off in parseAuthority before parseHost ever
			// runs, and the one userinfo-side reason quotes three characters of a percent
			// escape - so this is the operator's own configuration rather than resolver text.
			// It is bounded to maxEndpointBytes all the same, and the wording now says what
			// actually happens.
			//
			// errors.AsType and not errors.As: same search, same first match, same assignment.
			// Pinned by TestNew/"withholds the endpoint and keeps only the bounded inner
			// reason", which builds its expectation from url.Parse's own *url.Error.
			reason := err.Error()
			if urlErr, ok := errors.AsType[*url.Error](err); ok {
				reason = urlErr.Err.Error()
			}

			return nil, fmt.Errorf("invalid secret resolver endpoint, it is not named because it can carry a credential, only the parser's own bounded reason is kept: %s", sanitize(reason, maxEndpointBytes))
		}

		// Refused, not carried. The endpoint comes from PORTAINER_SECRET_RESOLVER, and
		// libstack.PortainerEnvVars sweeps every PORTAINER_-prefixed variable of the server
		// process into the compose environment of every project, so a stack body that
		// interpolates that variable puts this endpoint into a container's Config.Env -
		// readable through the docker proxy by anyone who can edit a stack. A credential
		// placed here would therefore publish itself, and this package has no channel that
		// can carry one safely, so the configuration is refused rather than used.
		//
		// The refusal is at startup and not at the first deploy for the same reason a broken
		// timeout is: a misconfiguration that only surfaces mid-migration is the expensive
		// kind. redactEndpoint is what keeps the credential out of this error, which FromEnv
		// turns into configErr and every Fetch then persists as a deployment status message.
		if parsed.User != nil {
			return nil, fmt.Errorf("secret resolver endpoint %s carries a credential in its userinfo, which is refused: %s is interpolated into every compose project, so a credential in it is readable from any stack", redactEndpoint(endpoint), EndpointEnvVar)
		}

		if parsed.Host == "" {
			// Through redactEndpoint, which bounds it: "http:///" plus a mebibyte of path
			// parses, has no host, and used to be printed whole.
			return nil, fmt.Errorf("secret resolver endpoint %s has no host", redactEndpoint(parsed.String()))
		}

		return &Client{
			httpClient: &http.Client{
				// See refuseRedirects. This is the branch where its reason bites hardest: the
				// Location comes off a network hop, written by whatever answered.
				CheckRedirect: refuseRedirects,
				// See redirectReporter.
				Transport: &redirectReporter{base: &http.Transport{
					// No proxy, and stated rather than left to the zero value. An
					// &http.Client{} with no transport uses http.DefaultTransport, whose Proxy
					// is ProxyFromEnvironment - so an HTTP_PROXY or HTTPS_PROXY in the
					// Portainer container's environment would relay this call through the
					// proxy, and the resolver's response body *is* the secret values, in clear
					// text on a plain http:// endpoint. Verified: with HTTP_PROXY set at
					// process start, the proxy received the POST for an http:// endpoint,
					// while the unix:// branch - which has always built its own transport -
					// bypassed it. Go reads the proxy environment once per process, so this is
					// the ordinary container case, not an exotic one.
					//
					// A secret resolver is by design an adjacent or a local service, and its
					// answers are the values themselves: there is no scenario in which
					// relaying them through a proxy is intended.
					Proxy: nil,
				}},
			},
			url: strings.TrimRight(parsed.String(), "/") + resolvePath,
			// parsed cannot carry userinfo at all - the branch above refuses it - so this is
			// already redactEndpoint's own output; it goes through the function anyway,
			// because the invariant that every printed endpoint passes through one place is
			// what keeps this cheap to review.
			endpoint: redactEndpoint(parsed.String()),
			timeout:  timeout,
		}, nil

	default:
		return nil, fmt.Errorf(`unsupported secret resolver endpoint %s: expected "unix://<path>", "http://<host>" or "https://<host>"`, redactEndpoint(endpoint))
	}
}

// refuseRedirects makes http.Client.Do return a 3xx response as it stands instead of
// following it. Both endpoint forms use it; there is no form for which following is right.
//
// The design fact behind that: a secret resolver is an adjacent or a local service answering
// a POST whose response body *is* the secret values. It is not a web site, it has one
// endpoint, and there is no scenario in which being told to go and ask somebody else for the
// values is intended. Left at its zero value, CheckRedirect follows up to ten of them, and
// each hop opens a channel this package exists to keep shut.
//
// Resolver text into a persisted error. After a redirect req.URL is rebuilt from the
// resolver's Location header, and every error http.Client.Do then returns is a *url.Error,
// which prints that URL in full. Measured on both branches, with this refusal removed: a
// Location of 4096 bytes gave an error of 4168 bytes, 65536 gave 65608 and 1048576 gave
// 1048648, with the planted text inside - against a 124-byte transport-error baseline with no
// redirect, and against the 35 bytes the refused 3xx costs now. That error is wrapped into
// the stack's deployment status message, which Portainer persists and StackInspect serves. It
// walks past all three guards this package already has - maxResolverErrorBytes,
// truncateContentType, and the withholding of an undecodable body - because the text arrives
// through the URL and not through the body.
//
// With the redirect refused, the 3xx lands on Fetch's non-2xx branch, which names
// resp.StatusCode and the resolver's own bounded error field and nothing else. So no part of
// a Location, and no other resolver-chosen header, reaches THE RETURNED ERROR - which is the
// channel that matters, because Portainer persists it as the stack's deployment status
// message and StackInspect serves it back.
//
// That claim is about the returned error and is stated that narrowly on purpose, because two
// things do reach the server log, both deliberately and both bounded:
//
//   - logRefusedRedirect names the Location reduced to scheme, host and port, at most
//     maxRedirectTargetBytes of it. Without it a proxy in front of the resolver makes every
//     deploy fail with "status 308" and nothing to go on.
//   - Client.transportFailure logs at most maxTransportDetailBytes of net/http's own message,
//     which quotes resolver-written header text when a response cannot be parsed.
//
// The log is a lesser channel than the database - it is not served by the API, not persisted
// with the stack and not read by an agent - but it is not a free one, which is why both of
// those are bounded rather than merely non-fatal.
//
// # One margin that is worth knowing before anyone narrows the catch-all
//
// This function does not run first. http.Client.do parses the Location at the top of its
// redirect loop, BEFORE it consults CheckRedirect, and on a Location url.Parse refuses it
// returns `failed to parse Location header "<the whole Location>": <reason>` - the header
// verbatim, bounded by nothing below net/http's 10 MiB header limit. The refusal below never
// sees that response, and neither does Fetch.
//
// What holds it today is one branch and nothing else: that error is not a *net.OpError, not a
// *net.DNSError, not a TLS error and not a context sentinel, so classifyTransportFailure drops
// it into the catch-all, which is a fixed phrase. Measured with a mebibyte of unparsable
// Location: the returned error was 172 bytes, against 1048xxx if the text were wrapped.
//
// So the catch-all is load bearing here, and the cost of narrowing it is written down where
// somebody narrowing it will look: any new branch that returns text derived from err rather
// than a fixed phrase reopens this channel into Stack.DeploymentStatus[].Message, with no
// redirect followed and no header of ours involved. Narrow it by TYPE, never by text, and keep
// the last case fixed.
func refuseRedirects(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

// redirectReporter reports a refused redirect from the transport, which is the only place
// that sees every shape of one.
//
// logRefusedRedirect used to be called from Fetch, off the response http.Client.Do hands
// back - and for one shape Do hands back no response at all. Its redirect loop parses the
// Location before it consults CheckRedirect (see refuseRedirects), so a 3xx whose Location
// url.Parse refuses comes out as an error, the response is closed, and the warning never
// fired. That shape is not an exotic one: it is squarely inside the case the diagnostic
// exists for - a proxy in front of the resolver rewriting the request - and it was the one
// case in it that left the operator nothing at all, since the returned error is a fixed
// phrase by construction.
//
// A RoundTripper sees the response before any of that, so both shapes are reported here and
// the call is gone from Fetch. redactLocation answers "(unparsable location)" for the shape
// that used to be lost, and "(no location)" for a 300 or a 304, which carry none.
//
// The set of statuses is unchanged - every 3xx, not only the five net/http would act on -
// because a 3xx of any kind from a service whose only endpoint answers a POST with values is
// worth a line either way.
//
// # The margin this wrapper sits on, which is narrower than it looks
//
// The transport's error is returned UNTOUCHED, and it has to be. net/http's send() decides
// whether an https:// endpoint was answered in plain HTTP by looking at what the RoundTripper
// handed back - and it looks with a BARE TYPE ASSERTION, err.(tls.RecordHeaderError), not
// with errors.As. It then replaces that error with http.ErrSchemeMismatch, which is the
// sentinel classifyTransportFailure matches to report "the endpoint is https but the resolver
// answered in plain HTTP".
//
// A bare assertion does not unwrap. So a single fmt.Errorf("...: %w", err) added here - the
// ordinary reflex when wrapping a RoundTripper - hides the tls.RecordHeaderError from that
// assertion, the substitution never happens, and the commonest operator mistake this file has
// is silently reported as something else. That is the defect round 10 fixed, reintroduced from
// a different file.
//
// Measured, by adding exactly that wrap on a copy and running the package: one subtest
// reddened and only one -
// TestFetchClassifiesATransportFailure/"an https endpoint in front of something that is not
// TLS"/"a plain HTTP answer" - and the reason came back as "the endpoint is https but the
// resolver did not answer with TLS". Worth knowing precisely, because it is not the catch-all
// and is therefore worse than the catch-all would be: classifyTransportFailure's next branch
// matches tls.RecordHeaderError with errors.AsType, which DOES unwrap, so the wrapped error
// walks straight into the neighbouring case and the persisted message asserts confidently that
// the resolver did not answer with TLS - about an answer that was a perfectly good HTTP
// response. A demotion to the catch-all is at least vague; this one is specific and wrong.
//
// The same holds for any classification net/http or this package makes by bare TYPE rather
// than through errors.Is/As. The rule for this wrapper is therefore flat: observe the
// response, never touch the error. There is no diagnostic to add here that the log line in
// Client.transportFailure cannot carry instead.
type redirectReporter struct {
	base http.RoundTripper
}

func (t *redirectReporter) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		// Untouched, deliberately and load bearing. See the type doc.
		return resp, err
	}

	if resp.StatusCode >= http.StatusMultipleChoices && resp.StatusCode < http.StatusBadRequest {
		logRefusedRedirect(resp.StatusCode, resp.Header.Get("Location"))
	}

	return resp, nil
}

// CloseIdleConnections forwards to the base transport, restoring what wrapping it took away.
//
// http.Client.CloseIdleConnections does not call a method on an interface: it type-asserts
// its Transport to an unexported interface{ CloseIdleConnections() } and does nothing at all
// when the assertion fails. Wrapping *http.Transport in a type that does not carry the method
// therefore turned Client.CloseIdleConnections() into a silent no-op - no error, no log line,
// just connections held open.
//
// Nothing in Portainer calls it today: this client is built once per process and lives as long
// as the server. That is the reason the gap was survivable, not a reason to leave it - the
// caller who adds the call later would have no way to see that it does nothing. Six lines and
// a test cost less than that discovery. TestRedirectReporterClosesIdleConnections is the pin,
// and it drives the method through http.Client rather than directly, because the type
// assertion is the part that can regress.
func (t *redirectReporter) CloseIdleConnections() {
	if closer, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

// logRefusedRedirect reports a 3xx that refuseRedirects did not follow.
//
// Refusing is right and stays (see refuseRedirects), but the operator's side of it was
// "secret resolver returned status 308" with nothing to act on - and the way this fires in
// practice is not an attack: it is nginx, Traefik or Apache in front of the resolver doing
// trailing-slash canonicalisation on /v1/resolve, or an http->https upgrade. Every deploy
// then fails, identically, with no hint that the request is being sent somewhere else.
//
// So the target is named, in the log only, reduced to scheme, host and port and bounded on
// top of that. The path and the query are dropped because they are the parts a resolver can
// stretch to a mebibyte and use as a channel; the userinfo because redactLocation applies
// redactEndpoint's rule. The host that is left is resolver-written text of the same kind, so
// it is sanitised and bounded like every other piece of it. Nothing of this reaches the
// returned error.
//
// Called from redirectReporter and from nowhere else; see that type for why it is not called
// from Fetch any more.
func logRefusedRedirect(status int, location string) {
	log.Warn().
		Int("status", status).
		Str("location", sanitizeRedirectTarget(redactLocation(location))).
		Msg("Secret resolver answered with a redirect, which is never followed, check for a proxy in front of the resolver rewriting the request")
}

type resolveRequest struct {
	Version int      `json:"version"`
	Refs    []string `json:"refs"`
}

type resolveResponse struct {
	Values map[string]string `json:"values"`
	Error  string            `json:"error"`
}

// Fetch resolves every reference in one batched request and returns a value for
// each of them. It is all values or an error: a partial result would leave a
// service running with a garbage credential, which is worse than a failed deploy.
//
// Fetching is batched because one backend refresh (a Vaultwarden sync pulls the
// whole vault) costs the same whether one reference is needed or twenty.
//
// No error returned from here may ever contain a resolved value: a failed deploy's
// error string is persisted in Portainer's database and served back over the API.
func (c *Client) Fetch(ctx context.Context, refs []string) (map[string]string, error) {
	if c == nil {
		return nil, errors.New("secret resolver is not configured")
	}

	if c.configErr != nil {
		return nil, c.configErr
	}

	if len(refs) == 0 {
		return map[string]string{}, nil
	}

	// One vault entry is deliberately referenced from several variables and several
	// stacks, so the same reference commonly appears more than once.
	unique := deduplicate(refs)

	payload, err := json.Marshal(resolveRequest{Version: protocolVersion, Refs: unique})
	if err != nil {
		return nil, fmt.Errorf("failed to encode the secret resolver request: %w", err)
	}

	// The call gets its own short deadline instead of inheriting the deploy's, which
	// can legitimately run for an hour while images are pulled. A wedged resolver has
	// to fail the deploy in seconds, not hang it.
	//
	// The parent is kept in hand: it is the only way to tell this deadline firing from the
	// deploy itself going away, which classifyTransportFailure has to distinguish.
	callCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, c.url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("failed to build the secret resolver request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	log.Debug().Int("references", len(unique)).Msg("Resolving secret references")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// Classified rather than wrapped, and that is the whole point: see transportError.
		return nil, c.transportFailure(ctx, "failed to reach", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// One byte past the cap, so a body of exactly maxResponseBytes - a legitimate
	// response - is read in full and is still distinguishable from a truncated one.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		// The same treatment as the round trip above, and for the same reason. A read that
		// fails here is a transport failure that happened to surface one call later - the
		// headers parsed, the body did not arrive - so it is classified by the same closed
		// set rather than wrapped. Chunked-framing errors are the characteristic case, and
		// the body being read IS the secret values.
		return nil, c.transportFailure(ctx, "failed to read the answer of", err)
	}

	// A limit reader truncates silently, and the truncated body then fails to parse
	// with the same "unexpected end of JSON input" a corrupt response gives. Say which
	// of the two it was, otherwise the cap is undiagnosable.
	if len(body) > maxResponseBytes {
		return nil, fmt.Errorf("secret resolver response exceeds %d bytes", maxResponseBytes)
	}

	var parsed resolveResponse

	if resp.StatusCode != http.StatusOK {
		// Only the resolver's own error field is echoed; the raw body is never
		// included, since on a confused resolver it could hold a value. The parse error
		// is discarded rather than reported for the same reason - see below.
		//
		// The status is the number and never resp.Status, which Go fills from the status
		// line as the resolver wrote it: the reason phrase in it is the resolver's text,
		// capped only by net/http's 10 MiB header limit, and on an http:// endpoint it is
		// settable by anyone on the path. This error is persisted as the stack's
		// deployment status message, so quoting the phrase would defeat
		// maxResolverErrorBytes through the field next door, on every retry.
		//
		// A 3xx arrives here too, because refuseRedirects hands the redirect response back
		// rather than following it. Nothing here reads resp.Header, so the Location - which
		// is resolver-written text of the same unbounded kind as the reason phrase - never
		// reaches the error returned from here. Keep it that way: it is the widest of the
		// three, since following it would also rebuild req.URL out of it. The log gets the
		// scheme, host and port of it and nothing more, written by redirectReporter on the
		// way out of the transport rather than from here; see that type for why it moved.
		if err := json.Unmarshal(body, &parsed); err == nil && parsed.Error != "" {
			return nil, fmt.Errorf("secret resolver returned status %d: %s", resp.StatusCode, sanitizeResolverError(parsed.Error))
		}

		return nil, fmt.Errorf("secret resolver returned status %d", resp.StatusCode)
	}

	if err := json.Unmarshal(body, &parsed); err != nil {
		// The parse error is deliberately not wrapped, and not logged either. A JSON
		// decoder quotes the input it choked on: segmentio/encoding builds its syntax
		// errors as "json: <message>: <first 32 bytes of the buffer>". The buffer here is
		// the response body - {"values":{"<ref>":"<value>"... - so for a short reference
		// those 32 bytes reach into the value itself. That error is wrapped up the stack
		// into the stack's deployment status message, which Portainer persists and serves
		// back over the API, so it is a publication channel rather than a log line.
		//
		// What the diagnosis actually needs is not the malformed text but the fact that
		// the resolver answered 200 with something that is not the agreed JSON, and that
		// goes to the server log as non-content facts only.
		//
		// The status is the number and the content type is bounded, for the reason given
		// on the non-2xx branch above: both are resolver-written header text, and a log
		// line is a lesser channel than the database but not a free one.
		log.Error().
			Int("status", resp.StatusCode).
			Str("content_type", sanitizeContentType(resp.Header.Get("Content-Type"))).
			Int("body_bytes", len(body)).
			Msg("Secret resolver returned a response that could not be decoded, its content is withheld because it may contain resolved values")

		return nil, errors.New("failed to decode the secret resolver response, see the Portainer server log")
	}

	// Defence in depth against a second implementation of the wire contract rather
	// than a check the current resolver can trip: it never puts an error field in a
	// 200. An implementation that did, with no values, would otherwise be reported as
	// "no value for reference X", hiding the reason - a locked vault, say.
	if parsed.Error != "" {
		return nil, fmt.Errorf("secret resolver returned an error: %s", sanitizeResolverError(parsed.Error))
	}

	values := make(map[string]string, len(unique))

	for _, ref := range unique {
		// An empty value is treated as a missing one. This does not fire against the
		// current resolver, which refuses an empty value in the backend, but the check
		// belongs to the contract rather than to that implementation: the mandatory
		// ${VAR:?message} form would fail the deploy anyway, yet nothing enforces that
		// form, and a bare ${VAR} would quietly start a service on a blank password.
		value, ok := parsed.Values[ref]
		if !ok || value == "" {
			// Bounded: a reference is a stack env value, so its length is whatever whoever
			// edited the stack put there, and this error is persisted as that stack's
			// deployment status message. %q does the escaping - see truncateReference.
			return nil, fmt.Errorf("secret resolver returned no value for reference %q", truncateReference(ref))
		}

		values[ref] = value
	}

	return values, nil
}

// transportError is a failed round trip to the resolver, reported as facts this process
// produced rather than as the text net/http hands back.
//
// The text is the problem. http.Client.Do reports a response it cannot parse by quoting the
// header the resolver wrote - `bad Content-Length "<the header>"`, `unsupported transfer
// encoding "<the header>"`, `malformed HTTP response "<the status line>"` - and net/http
// bounds none of that below MaxResponseHeaderBytes, 10 MiB. Wrapping it with %w carried the
// whole of it into an error that api/exec wraps again into the stack's deployment status
// message, which Portainer persists and StackInspect serves: measured on this package before
// this type existed, a mebibyte of planted Content-Length came back as an error of 1048724
// bytes with the planted text inside. It is the same publication channel the redirect
// refusal closed, reached without any redirect.
//
// So Error() is built from a fixed phrase, the configured endpoint and one of a closed set
// of reasons, and is bounded by construction rather than by truncation. Nothing derived from
// the resolver's text appears in it; a bounded copy of the real message goes to the log.
type transportError struct {
	message string

	// cause is what errors.Is sees, and it is only ever one of this process's own
	// sentinels - context.Canceled or context.DeadlineExceeded - never the transport error
	// itself. That asymmetry is deliberate and is the difference from api/exec's
	// withheldError, which unwraps to the error it withholds and therefore has to argue,
	// sink by sink, that nobody unwraps and prints it. Here there is nothing to argue
	// about: an extract-and-print sink anywhere above this error reaches a context sentinel
	// and nothing else, whatever is added to the call path later.
	cause error
}

// Error returns the constructed description. See the type doc for what is not in it.
func (e *transportError) Error() string { return e.message }

// Unwrap exposes the classified cause so that a caller can still tell "the deploy was
// cancelled" from "the resolver ran out of time" with errors.Is. It is nil for every other
// classification, because nothing else this branch can carry is safe to hand out.
func (e *transportError) Unwrap() error { return e.cause }

// transportFailure turns an error from the round trip into the error Fetch returns, and
// sends the detail to the server log.
//
// what names the step for the operator ("failed to reach", "failed to read the answer of")
// and is a literal at every call site; the endpoint is the operator's own configuration,
// reduced by redactEndpoint; the reason comes from the closed set in
// classifyTransportFailure. Those three are the whole of the returned message.
func (c *Client) transportFailure(parent context.Context, what string, err error) error {
	reason, cause := c.classifyTransportFailure(parent, err)

	// The one place net/http's own message is allowed to appear, bounded, and only here:
	// the log is not persisted with the stack, not served by the API and not read back by
	// an agent. Without it a resolver answering malformed HTTP is undiagnosable, since the
	// returned error deliberately says only which of the reasons it was.
	log.Error().
		Str("endpoint", c.endpoint).
		Str("reason", reason).
		Str("detail", sanitizeTransportDetail(err.Error())).
		Msg("Secret resolver call failed, the detail is bounded here and withheld from the returned error because it can quote resolver-written header text")

	// The pointer to the log line is the message's, not the caller's: the real reason lives
	// only there, by construction, and without the pointer an operator reading "its answer
	// could not be read as an HTTP response" has nothing to look up. The neighbouring decode
	// branch has said so since it was written; this one is the branch where the detail now
	// actually lives, so it says so too. The ceiling is unaffected - the addition is a
	// literal, and TestFetchBoundsATransportError still holds at 512 bytes.
	return &transportError{
		message: fmt.Sprintf("%s the secret resolver at %s: %s, see the Portainer server log", what, c.endpoint, reason),
		cause:   cause,
	}
}

// classifyTransportFailure reduces a transport error to one of a closed set of reasons and,
// where there is one, to a sentinel safe to unwrap.
//
// It reads the error's TYPE and this process's own state, never its text. The distinction
// the whole classification turns on: whether a fact was produced here - a deadline of ours
// fired, the caller went away, the connection was never opened, a certificate did not verify
// - or by the far end, which is never safe. Anything that got far enough for net/http to
// start parsing a response falls to the last case, where the reason is a fixed phrase,
// because that is exactly where the resolver's own header text lives.
func (c *Client) classifyTransportFailure(parent context.Context, err error) (string, error) {
	// The caller's context is checked first: when the deploy itself goes away, the context
	// derived from it reports the same cancellation, so the two are indistinguishable from
	// err alone - and "the resolver did not answer in time" would be a lie about a deploy
	// the operator cancelled.
	if parentErr := parent.Err(); parentErr != nil {
		if errors.Is(parentErr, context.DeadlineExceeded) {
			return "the deploy's own deadline expired", parentErr
		}

		return "the deploy was cancelled", parentErr
	}

	if errors.Is(err, context.DeadlineExceeded) {
		// The characteristic failure of a wedged resolver, and the reason DefaultTimeout
		// exists, so it names the budget that was exceeded rather than only the fact.
		return fmt.Sprintf("no answer within %s", c.timeout), context.DeadlineExceeded
	}

	// Defence in depth, and marked as such rather than left to look reachable. callCtx is
	// derived from parent and cancelled only by parent, by the deadline above, or by the
	// deferred cancel that runs after Fetch has returned - and Go's context propagates a
	// cancellation parent-first, so parent.Err() is already set whenever the parent is the
	// source. No path was found on which this branch fires. It stays because the alternative
	// to a wrong classification here is not "nothing" but the catch-all, which would assert
	// that the answer could not be read as an HTTP response - a statement about the resolver,
	// made about a cancellation of ours. Named in docs/secret-resolver.md §3.12 as one of the
	// closed set, with the same note.
	if errors.Is(err, context.Canceled) {
		return "the call was cancelled", context.Canceled
	}

	if _, ok := errors.AsType[*net.DNSError](err); ok {
		return "the endpoint's host name did not resolve", nil
	}

	// Before any response byte is read, so nothing in it is the resolver's text - but the
	// reason is still a fixed phrase, because a certificate's subject and SAN list are
	// chosen by whoever answers the socket and x509's errors quote them.
	if _, ok := errors.AsType[*tls.CertificateVerificationError](err); ok {
		return "the resolver's TLS certificate did not verify", nil
	}

	// The commonest operator mistake this file has: an https:// endpoint in front of a
	// resolver that serves plain HTTP. It does NOT arrive as the tls.RecordHeaderError below,
	// which is what the branch beneath used to claim. net/http checks the failed record header
	// itself and, when it spells "HTTP/", REPLACES the error with this sentinel
	// (net/http/client.go, send()) - so the type assertion never matched, the case fell to the
	// catch-all, and the persisted message then asserted something false: that the answer could
	// not be read as an HTTP response, when in fact it was a perfectly good HTTP response that
	// simply was not TLS.
	//
	// Matched by the sentinel net/http provides, which is the whole reason it provides one.
	// Never by the error's text: http.ErrSchemeMismatch is errors.New, so errors.Is compares
	// identity through the *url.Error that wraps it.
	if errors.Is(err, http.ErrSchemeMismatch) {
		return "the endpoint is https but the resolver answered in plain HTTP", nil
	}

	// What is left for the type check, and it is a real case rather than a leftover: a first
	// record that is neither a TLS record nor "HTTP/" - a resolver answering an https://
	// endpoint with something that is not HTTP at all. net/http leaves that one alone.
	if _, ok := errors.AsType[tls.RecordHeaderError](err); ok {
		return "the endpoint is https but the resolver did not answer with TLS", nil
	}

	if opErr, ok := errors.AsType[*net.OpError](err); ok {
		// A dial failure is the commonest thing an operator hits - a resolver that is down,
		// a socket path that does not exist - and it is worth separating from a connection
		// that was established and then broke.
		if opErr.Op == "dial" {
			return "the connection could not be opened", nil
		}

		return "the connection failed during the exchange", nil
	}

	return "its answer could not be read as an HTTP response", nil
}

// sanitizeResolverError sanitises and bounds the resolver's error field before it is
// echoed into an error of ours. See maxResolverErrorBytes for both halves of why.
func sanitizeResolverError(text string) string {
	return sanitize(text, maxResolverErrorBytes)
}

// sanitizeTransportDetail sanitises and bounds net/http's own message before it is logged.
// See maxTransportDetailBytes for why it is bounded separately from the other channels;
// the message quotes header text the resolver wrote, so it is sanitised for the same
// reason the error field is.
func sanitizeTransportDetail(text string) string {
	return sanitize(text, maxTransportDetailBytes)
}

// sanitizeRedirectTarget sanitises and bounds a reduced Location before it is logged. See
// maxRedirectTargetBytes, and redactLocation for the reduction that happens first.
func sanitizeRedirectTarget(text string) string {
	return sanitize(text, maxRedirectTargetBytes)
}

// sanitizeContentType sanitises and bounds the resolver's Content-Type header before it is
// logged. See maxContentTypeBytes for why it is bounded separately from the error field.
func sanitizeContentType(text string) string {
	return sanitize(text, maxContentTypeBytes)
}

// truncateReference bounds a reference before it is named in an error. Length only: every
// site that prints one does so with %q, which escapes what sanitize would escape. See
// maxReferenceBytes.
func truncateReference(ref string) string {
	return truncate(ref, maxReferenceBytes)
}

// truncateTimeoutValue bounds the raw timeout environment value before it is quoted back at
// the operator. Length only, for the same reason as truncateReference.
func truncateTimeoutValue(raw string) string {
	return truncate(raw, maxTimeoutValueBytes)
}

// TruncateName bounds a name out of a stack's own configuration - the stack's, or one of its
// variables' - before api/* formats it into an ERROR. Errors are the whole of its scope: every
// site that names a stack or a variable in an error calls this and prints the result with %q,
// while the four log statements that name a stack deliberately call nothing, for reasons given
// under "Why the log sites are outside this" below.
//
// # The class, stated once because it is handled the same way in four files
//
// Every error in api/exec and api/stacks/deployments that names the stack or the variable
// carrying a secret reference goes through here: the three refusals this feature adds - the
// non-administrator gate in api/stacks/deployments/deployment_compose_config.go, the
// compose-unpacker path in api/stacks/deployments/compose_unpacker_cmd_builder.go and the
// swarm path in api/exec/swarm_stack.go - plus the resolution failures and the withheld
// deploy error in api/exec/compose_stack.go. They get identical treatment because they are
// one channel: stackutils.UpdateStackStatusFromDeploymentResult writes whatever they say
// verbatim into Stack.DeploymentStatus[].Message, which Portainer persists and StackInspect
// serves back, where an AI agent doing ordinary infrastructure work reads it.
//
// Neither name is an identifier as far as this process is concerned. Both arrive in
// portainer.Stack out of a request body, and updateComposeStackPayload.Validate checks only
// that the compose file is non-empty - it says nothing about Env at all - so a variable name
// is arbitrary bytes of arbitrary length, and nothing in the server caps a request body
// either.
//
// A stack name is the narrower of the two but not by as much as it looks: normalizeStackName
// reduces it to [-_a-z0-9] on the create and update paths, which closes the byte channel
// there, and POST /stacks/{id}/migrate assigns and persists payload.Name without going
// through it at all. So %q is load bearing on both names rather than decorative on one, and
// neither is length-bounded anywhere upstream: a mebibyte of 'a' survives normalizeStackName
// whole.
//
// Measured with "NAME\x00\x1b[31m\n" + 'A'*1MiB in the variable name and an ordinary stack
// name, before this function existed:
//
//	refuseSecretReferencesForNonAdmin  1048755 bytes, with the NUL, the ESC and the newline
//	checkNoSecretReferences            1048732 bytes, same
//	SwarmStackManager.Deploy           1048700 bytes, same
//
// and a mebibyte in each of the two names put all three over 2 MiB.
//
// Escaped AND bounded, because either half alone is no fix: %q closes the control characters
// and leaves the mebibyte, the bound closes the mebibyte and leaves the ESC. This text is
// Portainer's own data rather than the resolver's, so it lands on the package rule's
// bounded-and-%q side rather than going through sanitize - see the package doc.
//
// # The bound is on the input of %q, so the printed name is larger
//
// maxNameBytes is what this function hands to %q, not what %q writes. %q renders an invalid
// UTF-8 byte and a C0 byte as four characters of \xNN, so a 256-byte name of either prints as
// 1029 bytes once the truncation marker and the quotes are counted, and a message naming both a
// stack and a variable reaches 2212 bytes rather than the 600-odd an all-printable name
// suggests. Anybody sizing a buffer, a column or a test ceiling off maxNameBytes has to
// multiply by four. truncate carries the full table and the contrast with sanitize, which
// bounds its output instead.
//
// # Why it is worth a call at every one of those sites
//
// Written here because it is the argument somebody deleting one of the calls has to answer.
// refuseSecretReferencesForNonAdmin fires precisely FOR a user who is not an administrator -
// the lowest-privileged actor in this feature's threat model, and the one the gate exists to
// contain - while resolver-chosen text, which comes from our own adjacent component, has gone
// through sanitize since round 10. Unbounded and unescaped on the low-privilege user's side
// and sanitised on ours is the priorities inverted.
//
// It lives in this package rather than in api/* because api/* already depends on
// pkg/secretresolver and the reverse edge does not exist: putting it here lets all four files
// share one function and one bound without anyone adding one.
//
// # Why the log sites are outside this
//
// Four log statements in three functions name a stack and call neither this function nor
// anything else: LogEscapedValues in this file, reached from all three deploy paths;
// logWithheldError's two branches in api/exec/compose_stack.go; and warnAboutStaleEnvFile in
// the same file. That is a decision rather than an oversight, and it is recorded here because
// this is where somebody will come looking for the rule.
//
// The log is a smaller channel than the error, and that is the whole of the argument. A refusal
// is written into Stack.DeploymentStatus[].Message by
// stackutils.UpdateStackStatusFromDeploymentResult, persisted in the database, and served back
// by StackInspect to anything holding a token - an operator, a CI job, an agent - which is what
// makes a call worth it at every error site. A log line goes to the server's stderr, is stored
// with nothing, is served by no API, and is already visible only to somebody who can read every
// stack's name anyway.
//
// Vanilla Portainer opens that same channel wider than this feature does, on paths this feature
// does not touch: api/http/handler/stacks/create_compose_stack.go logs
// fmt.Sprintf("%+v", stack) - the whole struct, name, env pairs and all - and
// api/stacks/deployments/deploy.go logs Str("stack", stack.Name) unbounded on the auto-update
// path. These four statements use an existing channel rather than opening a new one, so
// bounding only our own four would buy nothing and would imply the channel is bounded.
//
// What the log does with the bytes was measured rather than assumed, against zerolog v1.34.0
// with "STACKNAME\x00\x1b[31m\n" plus a mebibyte of 'A' in the field. It splits the two halves
// of the problem apart, and not where one would guess:
//
//   - Escaping is NOT a property of JSON mode alone, as one might expect from the fact that
//     api/cli/cli.go defaults --log-mode to PRETTY and api/logs/log.go turns that into a
//     zerolog.ConsoleWriter that decodes the JSON back. ConsoleWriter escapes too: writeFields
//     puts every string field through needsQuote - true for any byte below 0x20, above 0x7e, or
//     a space, backslash or quote - and then strconv.Quote. PRETTY and NOCOLOR printed
//     stack="STACKNAME\x00\x1b[31m\nAAA...", with no raw NUL, ESC or newline reaching the
//     terminal; JSON printed \u0000 and \u001b. None of that is ours, though: it is zerolog's
//     behaviour, pinned by no test of this fork, and it would change if ConsoleWriter dropped
//     needsQuote.
//   - Length is genuinely unbounded, in all three modes: the same measurement produced log lines
//     of 1048698 to 1048734 bytes, the whole mebibyte. That is the half a caller can actually
//     reach here, and it is the half being accepted. A log line is rotated and discarded; the
//     1048755-byte refusal measured above stayed in the database until the stack was deleted.
//
// All of that is a property of a FIELD, and every one of these four statements puts the name in
// one - Str("stack", name), beside a Msg whose text is a literal. That is load-bearing rather
// than incidental: needsQuote and strconv.Quote are applied by writeFields, and nothing escapes
// anything on the message path. Portainer replaces zerolog's message formatter with
// api/logs/log.go's formatMessage, which is fmt.Sprintf("%s |", i) and copies the bytes through,
// and leaves FormatFieldValue alone. So folding a name into Msgf - the obvious-looking tidy-up,
// one call instead of two - would put a raw NUL and a raw ESC on an operator's terminal and void
// the paragraph above. A name goes in a field, or it goes through TruncateName.
//
// So the rule is: an error naming a stack or a variable goes through this function, a log line
// naming one need not. Anything that starts persisting or serving a log line moves it into the
// first category and brings the call with it.
func TruncateName(name string) string {
	return truncate(name, maxNameBytes)
}

// hexDigits indexes the two nibbles of an escaped byte.
const hexDigits = "0123456789abcdef"

// sanitize replaces every C0 and C1 control character, every DEL and every byte that is not
// valid UTF-8 with a printable escape, and bounds the result to limit bytes, marking it when
// anything was dropped.
//
// # Escaping rather than stripping
//
// Stripping is simpler and it was rejected, for two reasons that both matter here.
//
// Evidence: this text is the resolver's own diagnosis and it is the only thing an operator
// has to go on when the vault is locked. A message that arrived with a NUL in it is a broken
// resolver, and silently deleting the NUL turns a diagnosable fault into a puzzling one.
//
// Injectivity, which is the sharper one: stripping joins whatever sat on either side of what
// it removed, so "SYS\x00TEM" strips to "SYSTEM" - a string the resolver never sent, now
// indistinguishable from one it did. An escape never merges two fragments. The backslash is
// escaped too, so that a resolver writing the literal six characters \x1b[ cannot be confused
// with one writing a real ESC, and the encoding stays reversible.
//
// # Why escaping cannot inflate anything past the bound
//
// The bound is applied to the OUTPUT, not to the input, and the input is read only as far as
// the output allows. A 512-byte field of newlines therefore yields 512 bytes of "\n" pairs
// and a marker, never 1024 bytes: sanitising and bounding are the same pass, so there is no
// order in which one can defeat the other.
//
// This is the opposite of truncate, whose limit bounds what the caller's %q is handed rather
// than what it prints, and which therefore inflates by up to four. Neither is a defect in the
// other: only a function that escapes can bound what escaping produces, and truncate does not
// escape because its callers' verb already does. See truncate for which channel gets which.
//
// The escape forms are chosen so that they cannot collide. A byte that is not valid UTF-8 at
// all is written \xNN - it is a byte, not a code point - while a C1 control, which is a
// perfectly valid two-byte rune, is written \u00NN.
func sanitize(text string, limit int) string {
	var out strings.Builder

	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])

		var escaped string

		switch {
		case r == utf8.RuneError && size == 1:
			// A lone continuation byte, an overlong encoding, a surrogate half or a
			// multi-byte sequence cut short by the end of the string. The offending byte
			// is printed rather than the replacement character, so the evidence survives.
			escaped = `\x` + hexByte(text[i])
		case r == '\\':
			escaped = `\\`
		case r == '\n':
			escaped = `\n`
		case r == '\r':
			escaped = `\r`
		case r == '\t':
			escaped = `\t`
		case r < 0x20 || r == 0x7f:
			// The rest of C0, and DEL. ESC lands here, which is the point: an ANSI CSI
			// sequence in a message an operator pastes into a terminal is a control
			// channel of its own.
			escaped = `\x` + hexByte(byte(r))
		case r >= 0x80 && r <= 0x9f:
			// C1. Reachable as a valid rune, and a terminal in an 8-bit mode reads 0x9b
			// as CSI directly.
			escaped = `\u00` + hexByte(byte(r))
		}

		if escaped == "" {
			if out.Len()+size > limit {
				return out.String() + truncationMarker
			}

			out.WriteString(text[i : i+size])
			i += size

			continue
		}

		if out.Len()+len(escaped) > limit {
			return out.String() + truncationMarker
		}

		out.WriteString(escaped)
		i += size
	}

	return out.String()
}

// hexByte renders one byte as two lowercase hex digits.
func hexByte(b byte) string {
	return string([]byte{hexDigits[b>>4], hexDigits[b&0x0f]})
}

// truncate cuts text to at most limit bytes, marking it when anything was dropped.
//
// Length only, for text whose printing verb already escapes it. Resolver-chosen text goes
// through sanitize instead.
//
// # The bound is on the INPUT of the printing verb, and %q inflates
//
// This is the one thing to know before reading a limit here as a message size. The caller
// truncates and then hands the result to %q, so limit bounds what %q is given, not what %q
// writes: an invalid UTF-8 byte and a C0 byte each come back out as four characters of \xNN, a
// backslash as two, and an ordinary printable as one. A bounded name is therefore between
// limit and roughly 4*limit bytes once printed.
//
// Measured at maxNameBytes = 256, with names of a single repeated byte and the printed form
// including the truncation marker and the two quotes:
//
//	invalid UTF-8, NUL   1029   four printed bytes per input byte
//	U+2028                515   two per input byte; 85 whole runes fit in 256
//	backslash             517   two per input byte
//	ordinary printable    261   one per input byte
//
// So the two messages that name both a stack and a variable reach 2212 bytes
// (refuseSecretReferencesForNonAdmin) and 2189 (checkNoSecretReferences) in the worst case,
// against 1188 and 1165 for a backslash and well under half that for plain text. The hostile
// fixtures in api/exec and api/stacks/deployments keep a readable prefix in each name, so they
// land a little under those figures - 2136 and 2113 - which is the closest a message that
// still says what it is about can get to the ceiling.
//
// Anything sizing a channel off one of these limits has to multiply. The ceilings in
// api/exec/compose_stack_secrets_test.go and api/stacks/deployments/deploy_test.go do, and
// derive the factor from strconv.Quote rather than from this table, so that retuning
// maxNameBytes cannot leave them stale.
//
// The order is nevertheless the right way round, and the alternative is worse: truncating
// AFTER %q could cut an escape sequence in half and leave a trailing "\x1" - a bound applied to
// the output of an escaping verb has to be applied by that verb, which is exactly what sanitize
// does and why it can promise what it promises.
//
// # How this differs from sanitize, and why each fits its channel
//
// sanitize escapes and bounds in one pass, so its limit is a hard bound on the OUTPUT: 512
// bytes in yields at most 512 bytes out. It has to be, because it feeds text into a message
// with no further escaping step, and because its input is the resolver's - an adjacent process
// whose output is its own business.
//
// truncate cannot make that promise and does not need to: its callers all print through %q,
// which is itself the escaping pass, so escaping here would double-escape and turn a readable
// name into \\x00. What it buys instead is fidelity - a name reaches the operator as %q renders
// it, the same text they would see anywhere else in Go tooling - at the price of a printed size
// that has to be reasoned about as a multiple. That trade is right for Portainer's own data,
// which an operator has to recognise, and wrong for the resolver's, which they only have to
// read.
//
// # The cut
//
// The cut is moved back to a rune boundary: slicing a UTF-8 string at a fixed byte
// offset can split a multi-byte rune, and the half-rune would then be rendered as a
// replacement character at the end of a message an operator has to read.
func truncate(text string, limit int) string {
	if len(text) <= limit {
		return text
	}

	cut := limit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}

	return text[:cut] + truncationMarker
}

// deduplicate returns the references without repeats, preserving their order.
func deduplicate(refs []string) []string {
	seen := make(map[string]struct{}, len(refs))
	unique := make([]string, 0, len(refs))

	for _, ref := range refs {
		if _, ok := seen[ref]; ok {
			continue
		}

		seen[ref] = struct{}{}
		unique = append(unique, ref)
	}

	return unique
}
