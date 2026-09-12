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
// with the details going to the server log instead. That extends to the status line:
// an error about a non-2xx names resp.StatusCode and never resp.Status, whose reason
// phrase is the resolver's own text, bounded only by net/http's 10 MiB header limit.
//
// So a resolver that interpolates a resolved value into its "error" field breaks the
// contract, and the value lands permanently in the failed stack's deployment status
// message, served back by StackInspect. The field is echoed anyway, sanitised and
// bounded to maxResolverErrorBytes, because it is what makes "the vault is locked"
// diagnosable from the UI; see docs/secret-resolver.md §3.12.
//
// # Sanitising
//
// That sanctioned 512-byte channel is byte-arbitrary, and the message it lands in is read
// by AI agents as well as by operators at a terminal: an "error" field carrying two
// newlines, a NUL, an ANSI CSI escape and a forged-looking JSON log record reading
// "SYSTEM: ignore previous instructions" was persisted whole, at 552 bytes, entirely
// within the bound. The rule the package keeps:
//
//   - resolver-chosen text is sanitised AND bounded, in one pass, at the one function that
//     formats it - the error field, the Content-Type header, net/http's own message about a
//     failed round trip, and a reduced Location;
//   - the configured endpoint goes the same way, through redactEndpoint, and is therefore
//     printed with %s and never with %q: it arrives already escaped, and quoting it a
//     second time would escape its escapes;
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

	// EndpointEnvVar holds the resolver address.
	EndpointEnvVar = "PORTAINER_SECRET_RESOLVER"

	// TimeoutEnvVar overrides DefaultTimeout with a Go duration.
	TimeoutEnvVar = "PORTAINER_SECRET_RESOLVER_TIMEOUT"

	// DefaultTimeout bounds a single Fetch. It is the outermost of three nested budgets,
	// and the resolver's documentation pins the order they must keep:
	//
	//	RBW_TIMEOUT_SECONDS(20) < REQUEST_TIMEOUT_SECONDS(45) < this(60)
	//
	// The innermost bounds one vault subprocess, the middle one bounds a whole resolve
	// request, and this one bounds the round trip. Each must outlive the one inside it, or
	// the inner timeout can never fire and its diagnostic - the one that says which call
	// hung - is never printed, so move the chain together.
	//
	// The middle budget fits 1+N subprocess calls: a worst-case sync of 20 s plus 11.8 s of
	// process-spawn overhead at MAX_REFS=256 - 257 spawns at about 46 ms each, measured on an
	// idle eight-core box - is 31.8 s. Nothing is added for network latency: "rbw get" reads a
	// local cache and never contacts the server.
	DefaultTimeout = 60 * time.Second
)

const (
	// resolvePath is the resolver's only endpoint.
	resolvePath = "/v1/resolve"

	// protocolVersion lets the resolver evolve its wire format without a change on
	// the Portainer side.
	protocolVersion = 1

	// maxResponseBytes caps the response read so a broken resolver cannot balloon the
	// memory of the Portainer server. Enforced twice: the io.LimitReader in Fetch decides
	// how much is READ, and the length check beside it turns the result into a message that
	// says which of "oversized" and "corrupt" it was.
	maxResponseBytes = 1 << 20 // 1 MiB

	// maxResolverErrorBytes bounds the resolver's own error field, the only external text
	// this client ever puts into an error of its own. That error is persisted as the stack's
	// deployment status message and served back over the API, so a resolver that answers with
	// a stack trace or a page of vault output must not be able to write it into the database.
	// The field is byte-arbitrary within that bound, so it is sanitised as well; see sanitize.
	maxResolverErrorBytes = 512

	// maxEndpointBytes bounds the endpoint as configured - and url.Parse's own reason for
	// refusing one - wherever either reaches an error or a log line. Measured with a
	// mebibyte in PORTAINER_SECRET_RESOLVER, before and after:
	//
	//	transport error, unix endpoint       1048698 -> 364 bytes
	//	configErr, unparsable port           1048735 -> 390 bytes
	//	configErr, endpoint with no host     1048621 -> 296 bytes
	//	configErr, unsupported scheme        1048682 -> 359 bytes
	//
	// Three of those four are configErr, which EVERY Fetch returns and Portainer persists as
	// the stack's deployment status message, so one mistyped environment variable publishes
	// its bytes per deploy of every stack that uses references, rather than once.
	//
	// 256 bytes is a socket path, a host and a port with room to spare.
	maxEndpointBytes = 256

	// maxTimeoutValueBytes bounds the raw PORTAINER_SECRET_RESOLVER_TIMEOUT value before it
	// is quoted back at the operator, on the same configErr channel as maxEndpointBytes.
	maxTimeoutValueBytes = 64

	// maxReferenceBytes bounds a reference named in an error. A reference is a stack env
	// value, so its length is whatever whoever edited the stack put there - and the error
	// naming it is persisted as that stack's deployment status message.
	maxReferenceBytes = 256

	// maxNameBytes bounds a stack's name, or the name of one of its variables, before
	// api/* quotes it into a deploy error. See TruncateName for the channel.
	maxNameBytes = 256

	// maxContentTypeBytes bounds the resolver's Content-Type header before it is logged.
	// It is a separate number from maxResolverErrorBytes even though the two happen to
	// agree: they bound different channels - the error field goes to the database, the
	// header only to the log - and retuning one must not silently retune the other.
	maxContentTypeBytes = 512

	// maxTransportDetailBytes bounds net/http's own message about a failed round trip
	// before it is logged. net/http builds its response-parse errors by quoting the header
	// text the resolver wrote, verbatim and in full: a bad Content-Length, a duplicated
	// one, an unsupported Transfer-Encoding and a malformed status line are all reported
	// as `<what> "<the offending header>"`, bounded by nothing below
	// MaxResponseHeaderBytes at 10 MiB. Measured with the header planted at a mebibyte,
	// the returned error was 1048724 bytes and carried the planted text whole. None of
	// that may reach the returned error, which Portainer persists as the stack's
	// deployment status message; a bounded copy in the log is what keeps a genuinely
	// broken resolver diagnosable at all.
	maxTransportDetailBytes = 512

	// maxRedirectTargetBytes bounds the reduced Location of a refused redirect before it
	// is logged - see logRefusedRedirect. Reducing the Location to scheme, host and port
	// already drops the room a resolver has to write free text into it, but a host name is
	// resolver-written text too and nothing below the 10 MiB header limit bounds it.
	//
	// The host position is the one a test has to drive: redactLocation drops the path
	// BEFORE this bound is applied, so a mebibyte planted in the path is gone either way
	// and a probe built on one is falsely green. Measured with the mebibyte in the HOST
	// instead and this bound removed, the log line was 1048872 bytes.
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
// The escape exists because the marker is a bare prefix: without it a stack variable whose
// value legitimately begins with "secret:" would be undeployable on this fork.
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
// gives it - so it gets a line of its own. The count and not the values.
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
	// piece of variable text the errors of a failed round trip are allowed to carry. It is
	// kept separately from url because url is a request URL with resolvePath appended.
	endpoint string

	// configErr carries a broken environment configuration. Dropping it would degrade a
	// misconfiguration into "no resolver configured" - the silent fallback that would let a
	// stack deploy with an unresolved reference - so it is kept and every Fetch fails with it.
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
			// Bounded, and the parser's own error dropped rather than wrapped: it quotes the
			// whole value a second time, bounding it no better than %q did.
			return misconfigured(fmt.Errorf("invalid %s value %q, expected a Go duration such as 60s", TimeoutEnvVar, truncateTimeoutValue(raw)))
		}

		timeout = parsed
	}

	client, err := New(endpoint, timeout)
	if err != nil {
		return misconfigured(err)
	}

	// Logged so a typo in the endpoint surfaces at startup rather than at the first deploy
	// of a migrated stack - but never the endpoint as given.
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
// This is the one place that rule lives: no URL of any kind is formatted into an error or a
// log line in this package without passing through here. The rule and nothing else - it
// applies no bound, because redactEndpoint and redactLocation bound different channels and a
// bound applied here would be whichever of the two is smaller, silently.
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
// Sanitising is belt and braces on top: url.Parse already refuses a URL containing an ASCII
// control byte, and URL.String percent-encodes what is left.
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
// It differs from redactEndpoint because the inputs differ. An endpoint is the operator's own
// configuration and its path is load bearing: for a unix:// endpoint the path IS the socket. A
// Location is text the resolver wrote, its path and query are free-text fields bounded only by
// net/http's 10 MiB header limit, and a host name is bounded by nothing below that either -
// hence the bound the caller applies on top.
func redactLocation(location string) string {
	if location == "" {
		// A 3xx is not obliged to carry a Location and 300 and 304 routinely do not. It
		// needs its own answer because url.Parse("") succeeds, with an empty scheme and an
		// empty host, and would therefore be reported below as "(relative location)".
		return "(no location)"
	}

	parsed, err := url.Parse(location)
	if err != nil {
		// Unparsable, so nothing in it can be classified as safe to print.
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
		// No userinfo refusal here, unlike the http(s) branch below: the dialer receives
		// everything after "unix://" as a filesystem path, so a "user:token@" in it is part of
		// that path and reaches nobody as a credential.
		//
		// The redactEndpoint calls in this branch do parse it, and that has a consequence worth
		// knowing: measured, "unix://user:token@/run/x.sock" dials the path
		// "user:token@/run/x.sock" while every message and log line naming this client says
		// "unix:///run/x.sock". The printed form is therefore not always the path in use.
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
				// See redirectReporter. Wrapped on both endpoint forms.
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
			// url.Parse fails with a *url.Error, which prints the URL it was handed in full:
			// the password masking that hides a credential in net/http's errors is applied by
			// net/http, not by the error type. So only the inner reason is kept and the
			// endpoint is not named. Its text is not entirely withheld either - url.Parse
			// builds its inner errors by quoting the fragment that failed, so a slice of the
			// endpoint rides out inside the reason, and nothing bounded it: measured with a
			// mebibyte in the port position, configErr came back at 1048735 bytes. No
			// credential reaches it, verified over a 38-input corpus: url.Parse splits the
			// userinfo off in parseAuthority before parseHost ever runs.
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
		// placed here would therefore publish itself. redactEndpoint is what keeps it out of
		// this error, which FromEnv turns into configErr and every Fetch then persists as a
		// deployment status message.
		if parsed.User != nil {
			return nil, fmt.Errorf("secret resolver endpoint %s carries a credential in its userinfo, which is refused: %s is interpolated into every compose project, so a credential in it is readable from any stack", redactEndpoint(endpoint), EndpointEnvVar)
		}

		if parsed.Host == "" {
			// Through redactEndpoint, which bounds it: "http:///" plus a mebibyte of path
			// parses and has no host.
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
					Proxy: nil,
				}},
			},
			url: strings.TrimRight(parsed.String(), "/") + resolvePath,
			// parsed cannot carry userinfo at all - the branch above refuses it - so this goes
			// through redactEndpoint only to keep every printed endpoint on one path.
			endpoint: redactEndpoint(parsed.String()),
			timeout:  timeout,
		}, nil

	default:
		return nil, fmt.Errorf(`unsupported secret resolver endpoint %s: expected "unix://<path>", "http://<host>" or "https://<host>"`, redactEndpoint(endpoint))
	}
}

// refuseRedirects makes http.Client.Do return a 3xx response as it stands instead of
// following it. A secret resolver is an adjacent or a local service answering a POST whose
// response body *is* the secret values; there is no endpoint form for which following a
// redirect is right, and left at its zero value CheckRedirect follows up to ten of them.
//
// The reason is resolver text reaching a persisted error: after a redirect req.URL is rebuilt
// from the resolver's Location header, and every error http.Client.Do then returns is a
// *url.Error, which prints that URL in full. Measured with this refusal removed, a Location of
// 4096 bytes gave an error of 4168 bytes, 65536 gave 65608 and 1048576 gave 1048648, with the
// planted text inside - against a 124-byte transport-error baseline with no redirect, and the
// 35 bytes the refused 3xx costs now. With the redirect refused, the 3xx lands on Fetch's
// non-2xx branch, which names resp.StatusCode and the resolver's own bounded error field and
// nothing else.
//
// # One margin that is worth knowing before anyone narrows the catch-all
//
// This function does not run first: http.Client.do parses the Location BEFORE it consults
// CheckRedirect, so a Location url.Parse refuses never reaches it and comes back as
// `failed to parse Location header "<the whole Location>": <reason>` - the header verbatim,
// bounded by nothing below net/http's 10 MiB header limit. What holds that today is
// classifyTransportFailure's catch-all, which is a fixed phrase: measured with a mebibyte of
// unparsable Location, the returned error was 172 bytes, against 1048xxx if the text were
// wrapped. Narrow that classification by TYPE, never by text, and keep the last case fixed.
func refuseRedirects(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

// redirectReporter reports a refused redirect from the transport, which is the only place
// that sees every shape of one: http.Client.Do hands back no response at all for a 3xx whose
// Location url.Parse refuses, because its redirect loop parses the Location before it consults
// CheckRedirect (see refuseRedirects). Every 3xx is reported, not only the five net/http would
// act on.
//
// # The margin this wrapper sits on, which is narrower than it looks
//
// The transport's error is returned UNTOUCHED, and it has to be. net/http's send() decides
// whether an https:// endpoint was answered in plain HTTP by looking at what the RoundTripper
// handed back - with a BARE TYPE ASSERTION, err.(tls.RecordHeaderError), not with errors.As -
// and replaces that error with http.ErrSchemeMismatch, the sentinel classifyTransportFailure
// matches. A bare assertion does not unwrap, so a single fmt.Errorf("...: %w", err) added here
// hides it. Measured, by adding exactly that wrap on a copy and running the package: the
// reason came back as "the endpoint is https but the resolver did not answer with TLS",
// because classifyTransportFailure's next branch matches tls.RecordHeaderError with
// errors.AsType, which DOES unwrap - a persisted message asserting confidently the wrong thing
// about an answer that was a perfectly good HTTP response.
//
// The rule for this wrapper is therefore flat: observe the response, never touch the error.
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
// http.Client.CloseIdleConnections type-asserts its Transport to an unexported
// interface{ CloseIdleConnections() } and does nothing at all when the assertion fails, so
// without this method it becomes a silent no-op.
func (t *redirectReporter) CloseIdleConnections() {
	if closer, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

// logRefusedRedirect reports a 3xx that refuseRedirects did not follow. The way this fires in
// practice is not an attack: it is nginx, Traefik or Apache in front of the resolver doing
// trailing-slash canonicalisation on /v1/resolve, or an http->https upgrade, and every deploy
// then fails identically with no hint that the request is being sent somewhere else.
//
// So the target is named, in the log only, reduced to scheme, host and port and bounded on top
// of that: the path and the query are the parts a resolver can stretch to a mebibyte and use as
// a channel. Nothing of this reaches the returned error.
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

	// The call gets its own short deadline instead of inheriting the deploy's, which can
	// legitimately run for an hour while images are pulled. The parent is kept in hand: it is
	// the only way to tell this deadline firing from the deploy itself going away, which
	// classifyTransportFailure has to distinguish.
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
		// A read that fails here is a transport failure that surfaced one call later - the
		// headers parsed, the body did not arrive - so it is classified by the same closed set
		// rather than wrapped. The body being read IS the secret values.
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
		// Only the resolver's own error field is echoed; the raw body is never included, since
		// on a confused resolver it could hold a value. The parse error is discarded rather
		// than reported for the same reason - see below.
		//
		// The status is the number and never resp.Status, which Go fills from the status line
		// as the resolver wrote it: the reason phrase in it is the resolver's text, capped only
		// by net/http's 10 MiB header limit, so quoting it would defeat maxResolverErrorBytes
		// through the field next door. A 3xx arrives here too, because refuseRedirects hands
		// the redirect response back rather than following it; nothing here reads resp.Header,
		// so the Location never reaches the error returned from here. Keep it that way.
		if err := json.Unmarshal(body, &parsed); err == nil && parsed.Error != "" {
			return nil, fmt.Errorf("secret resolver returned status %d: %s", resp.StatusCode, sanitizeResolverError(parsed.Error))
		}

		return nil, fmt.Errorf("secret resolver returned status %d", resp.StatusCode)
	}

	if err := json.Unmarshal(body, &parsed); err != nil {
		// The parse error is deliberately not wrapped, and not logged either. A JSON decoder
		// quotes the input it choked on: segmentio/encoding builds its syntax errors as
		// "json: <message>: <first 32 bytes of the buffer>". The buffer here is the response
		// body - {"values":{"<ref>":"<value>"... - so for a short reference those 32 bytes
		// reach into the value itself, and that error would be wrapped up the stack into the
		// deployment status message Portainer persists and serves.
		//
		// The log gets non-content facts only.
		log.Error().
			Int("status", resp.StatusCode).
			Str("content_type", sanitizeContentType(resp.Header.Get("Content-Type"))).
			Int("body_bytes", len(body)).
			Msg("Secret resolver returned a response that could not be decoded, its content is withheld because it may contain resolved values")

		return nil, errors.New("failed to decode the secret resolver response, see the Portainer server log")
	}

	// Defence in depth against a second implementation of the wire contract: the current
	// resolver never puts an error field in a 200, and one that did, with no values, would
	// otherwise be reported as "no value for reference X", hiding the reason - a locked
	// vault, say.
	if parsed.Error != "" {
		return nil, fmt.Errorf("secret resolver returned an error: %s", sanitizeResolverError(parsed.Error))
	}

	values := make(map[string]string, len(unique))

	for _, ref := range unique {
		// An empty value is treated as a missing one. Nothing enforces the mandatory
		// ${VAR:?message} form, and a bare ${VAR} would quietly start a service on a blank
		// password.
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
// The text is the problem: http.Client.Do reports a response it cannot parse by quoting the
// header the resolver wrote, unbounded below 10 MiB, and wrapping it with %w would carry the
// whole of it into the stack's deployment status message; see maxTransportDetailBytes for the
// measurement.
//
// So Error() is built from a fixed phrase, the configured endpoint and one of a closed set
// of reasons, and is bounded by construction rather than by truncation. Nothing derived from
// the resolver's text appears in it; a bounded copy of the real message goes to the log.
type transportError struct {
	message string

	// cause is what errors.Is sees, and it is only ever one of this process's own
	// sentinels - context.Canceled or context.DeadlineExceeded - never the transport error
	// itself. So an extract-and-print sink anywhere above this error reaches a context
	// sentinel and nothing else, whatever is added to the call path later.
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

	// The one place net/http's own message is allowed to appear, bounded: the log is not
	// persisted with the stack and not served by the API. Without it a resolver answering
	// malformed HTTP is undiagnosable.
	log.Error().
		Str("endpoint", c.endpoint).
		Str("reason", reason).
		Str("detail", sanitizeTransportDetail(err.Error())).
		Msg("Secret resolver call failed, the detail is bounded here and withheld from the returned error because it can quote resolver-written header text")

	// The message points at the log line because the real reason lives only there.
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

	// Defence in depth, and marked as such rather than left to look reachable: Go's context
	// propagates a cancellation parent-first, so parent.Err() is already set whenever the
	// parent is the source. It stays because the alternative here is not "nothing" but the
	// catch-all, which would assert that the answer could not be read as an HTTP response - a
	// statement about the resolver, made about a cancellation of ours.
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
	// resolver that serves plain HTTP. It does NOT arrive as the tls.RecordHeaderError below:
	// net/http checks the failed record header itself and, when it spells "HTTP/", REPLACES
	// the error with this sentinel (net/http/client.go, send()).
	//
	// Matched by the sentinel and never by the error's text: http.ErrSchemeMismatch is
	// errors.New, so errors.Is compares identity through the *url.Error that wraps it.
	if errors.Is(err, http.ErrSchemeMismatch) {
		return "the endpoint is https but the resolver answered in plain HTTP", nil
	}

	// What is left for the type check: a first record that is neither a TLS record nor
	// "HTTP/" - a resolver answering an https:// endpoint with something that is not HTTP at
	// all. net/http leaves that one alone.
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
// # The channel
//
// Every error in api/exec and api/stacks/deployments that names the stack or the variable
// carrying a secret reference goes through here, because they are one channel:
// stackutils.UpdateStackStatusFromDeploymentResult writes whatever they say verbatim into
// Stack.DeploymentStatus[].Message, which Portainer persists and StackInspect serves back,
// where an AI agent doing ordinary infrastructure work reads it.
//
// Neither name is an identifier as far as this process is concerned, and neither is
// length-bounded anywhere upstream: a variable name is arbitrary bytes out of a request body,
// and a mebibyte of 'a' survives normalizeStackName whole. Measured with
// "NAME\x00\x1b[31m\n" + 'A'*1MiB in the variable name and an ordinary stack name, with no
// bound applied:
//
//	refuseSecretReferencesForNonAdmin  1048755 bytes, with the NUL, the ESC and the newline
//	checkNoSecretReferences            1048732 bytes, same
//	SwarmStackManager.Deploy           1048700 bytes, same
//
// and a mebibyte in each of the two names put all three over 2 MiB.
//
// Escaped AND bounded, because either half alone is no fix: %q closes the control characters
// and leaves the mebibyte, the bound closes the mebibyte and leaves the ESC. maxNameBytes is
// what this function hands to %q, not what %q writes, so a bounded name prints as up to four
// times its length - truncate carries the table.
//
// # Why the log sites are outside this
//
// Four log statements in three functions name a stack and call nothing: LogEscapedValues in
// this file, logWithheldError's two branches in api/exec/compose_stack.go, and
// warnAboutStaleEnvFile in the same file. The log is a smaller channel than the error: a log
// line goes to the server's stderr, is stored with nothing, is served by no API, and is
// already visible only to somebody who can read every stack's name anyway.
//
// Measured against zerolog v1.34.0 with "STACKNAME\x00\x1b[31m\n" plus a mebibyte of 'A' in
// the field: escaping happens in every log mode, not only JSON - ConsoleWriter's writeFields
// puts every string field through needsQuote and then strconv.Quote, so no raw NUL, ESC or
// newline reached the terminal - while length is genuinely unbounded in all three modes, the
// same measurement producing log lines of 1048698 to 1048734 bytes. A log line is rotated and
// discarded; the 1048755-byte refusal measured above stayed in the database until the stack
// was deleted.
//
// All of that is a property of a FIELD, and every one of these four statements puts the name in
// one - Str("stack", name), beside a Msg whose text is a literal. Nothing escapes anything on
// the message path: Portainer replaces zerolog's message formatter with api/logs/log.go's
// formatMessage, which is fmt.Sprintf("%s |", i) and copies the bytes through, so folding a
// name into Msgf would put a raw NUL and a raw ESC on an operator's terminal.
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
// Escaping rather than stripping, because stripping joins whatever sat on either side of what
// it removed: "SYS\x00TEM" strips to "SYSTEM", a string the resolver never sent and now
// indistinguishable from one it did. The backslash is escaped too, so that a resolver writing
// the literal six characters \x1b[ cannot be confused with one writing a real ESC.
//
// The bound is applied to the OUTPUT, not to the input, and the input is read only as far as
// the output allows. A 512-byte field of newlines therefore yields 512 bytes of "\n" pairs and
// a marker, never 1024 bytes: sanitising and bounding are the same pass. This is the opposite
// of truncate, whose limit bounds what the caller's %q is handed rather than what it prints.
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
// The caller truncates and then hands the result to %q, so limit bounds what %q is given, not
// what %q writes: an invalid UTF-8 byte and a C0 byte each come back out as four characters of
// \xNN, a backslash as two, and an ordinary printable as one. Measured at maxNameBytes = 256,
// with names of a single repeated byte and the printed form including the truncation marker
// and the two quotes:
//
//	invalid UTF-8, NUL   1029   four printed bytes per input byte
//	U+2028                515   two per input byte; 85 whole runes fit in 256
//	backslash             517   two per input byte
//	ordinary printable    261   one per input byte
//
// So the two messages that name both a stack and a variable reach 2212 bytes
// (refuseSecretReferencesForNonAdmin) and 2189 (checkNoSecretReferences) in the worst case.
// Anything sizing a channel off one of these limits has to multiply. The ceilings in
// api/exec/compose_stack_secrets_test.go and api/stacks/deployments/deploy_test.go derive the
// factor from strconv.Quote rather than from this table, so that retuning maxNameBytes cannot
// leave them stale.
//
// The order is nevertheless the right way round: truncating AFTER %q could cut an escape
// sequence in half and leave a trailing "\x1" - a bound applied to the output of an escaping
// verb has to be applied by that verb, which is what sanitize does. Escaping here would
// double-escape instead, turning a readable name into \\x00.
//
// The cut is moved back to a rune boundary: slicing a UTF-8 string at a fixed byte offset can
// split a multi-byte rune, and the half-rune would then be rendered as a replacement character
// at the end of a message an operator has to read.
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
