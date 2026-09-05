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
// cap and the raw body behind a non-2xx status all produce an error built from
// non-content facts alone, with the details going to the server log instead.
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
// close. The field is echoed anyway, truncated to maxResolverErrorBytes, because it
// is what makes "the vault is locked" diagnosable from the UI; see the classification
// in docs/secret-resolver.md §3.12.
package secretresolver

import (
	"bytes"
	"context"
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
	//	RBW_TIMEOUT_SECONDS(20) < REQUEST_TIMEOUT_SECONDS(25) < this(30)
	//
	// The innermost bounds one vault subprocess, the middle one bounds a whole
	// resolve request, and this one bounds the round trip. Each must outlive the one
	// inside it, or the inner timeout can never fire and its diagnostic - the one
	// that says which call hung - is never printed. Lowering this number alone
	// silently disables the resolver's own error reporting, so move the chain
	// together.
	DefaultTimeout = 30 * time.Second
)

const (
	// resolvePath is the resolver's only endpoint.
	resolvePath = "/v1/resolve"

	// protocolVersion lets the resolver evolve its wire format without a change on
	// the Portainer side.
	protocolVersion = 1

	// maxResponseBytes caps the response read so a broken resolver cannot balloon
	// the memory of the Portainer server.
	maxResponseBytes = 1 << 20 // 1 MiB

	// maxResolverErrorBytes bounds the resolver's own error field, the only external
	// text this client ever puts into an error of its own. That error is persisted as
	// the stack's deployment status message and served back over the API, so its size
	// is Portainer's problem rather than the resolver's: a resolver that answers with a
	// stack trace or a page of vault output must not be able to write it into the
	// database. Bounding it is not a secrecy control - see the package doc for why the
	// field is trusted to be value-free - it is a control over how much of somebody
	// else's text Portainer stores and serves.
	maxResolverErrorBytes = 512

	// maxContentTypeBytes bounds the resolver's Content-Type header before it is logged.
	// It is a separate number from maxResolverErrorBytes even though the two happen to
	// agree: they bound different channels - the error field goes to the database, the
	// header only to the log - and retuning one must not silently retune the other. A
	// content type that needs more than this is not a content type.
	maxContentTypeBytes = 512

	// truncationMarker is appended to a resolver error cut short at
	// maxResolverErrorBytes, so a truncated message is not read as a complete one.
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

	// credential authenticates to the resolver. It is held here rather than left in url,
	// where net/http would derive the same Authorization header from it, because the URL
	// is what every transport error prints: http.Client.Do fails with a *url.Error built
	// from stripPassword(req.URL), and that masks the password and nothing else - a token
	// passed as "http://<token>@resolver:9100", which is the form this protocol offers,
	// comes back whole. That error is wrapped into the stack's deployment status message,
	// which Portainer persists and StackInspect serves. Kept out of the URL and applied
	// per request, the credential cannot reach any error text at all.
	credential *url.Userinfo

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
			return misconfigured(fmt.Errorf("invalid %s value %q: %w", TimeoutEnvVar, raw, err))
		}

		timeout = parsed
	}

	client, err := New(endpoint, timeout)
	if err != nil {
		return misconfigured(err)
	}

	// Logged so a typo in the endpoint surfaces at startup rather than at the first
	// deploy of a migrated stack - but never the endpoint as given. It can carry a
	// credential: "http://user:token@resolver:9100" parses, and its userinfo is in fact
	// the only way this protocol offers to authenticate to a remote resolver. New keeps
	// that userinfo out of the client's URL and applies it per request instead, so these
	// two lines are the last place where the endpoint as configured is in hand at all.
	safeEndpoint := redactEndpoint(endpoint)

	log.Info().Str("endpoint", safeEndpoint).Dur("timeout", timeout).Msg("Secret resolver configured")

	if isPlaintextRemote(endpoint) {
		log.Warn().Str("endpoint", safeEndpoint).Msg("Secret resolver endpoint is plain HTTP to a non-loopback host, resolved secret values will cross the network in clear text")
	}

	return client
}

// redactEndpoint returns the endpoint without its userinfo, which is the only part of it
// that can hold a credential. Everything a diagnosis needs - scheme, host, port, socket
// path - is kept.
//
// The whole userinfo goes, not url.Redacted()'s masked password: Redacted keeps the
// username, and a token passed as "http://<token>@resolver:9100" lives there.
//
// No endpoint as configured is ever formatted into an error or a log line in this
// package; every such site goes through here. Keeping that invariant whole is cheaper
// than re-deciding, site by site, whether this particular message can carry userinfo.
func redactEndpoint(endpoint string) string {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		// Unparsable, so nothing in it can be classified as safe to print.
		return "(unparsable endpoint)"
	}

	parsed.User = nil

	return parsed.String()
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
		socketPath := strings.TrimPrefix(endpoint, "unix://")
		if socketPath == "" {
			return nil, fmt.Errorf("secret resolver endpoint %q has no socket path", redactEndpoint(endpoint))
		}

		var dialer net.Dialer

		return &Client{
			httpClient: &http.Client{
				Transport: &http.Transport{
					DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
						return dialer.DialContext(ctx, "unix", socketPath)
					},
				},
			},
			// The host is ignored by the dialer above but net/http still requires a
			// syntactically valid URL.
			url:     "http://localhost" + resolvePath,
			timeout: timeout,
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
			// Portainer persists as the stack's deployment status message. Only the inner
			// reason is kept, and the endpoint is not named at all - it did not parse, so no
			// part of it can be classified as safe to print.
			var urlErr *url.Error
			if errors.As(err, &urlErr) {
				err = urlErr.Err
			}

			return nil, fmt.Errorf("invalid secret resolver endpoint, its text is withheld because it can carry a credential: %w", err)
		}

		// Taken out of the URL before anything below can quote it, and applied per request
		// in Fetch instead. See Client.credential.
		credential := parsed.User
		parsed.User = nil

		if parsed.Host == "" {
			return nil, fmt.Errorf("secret resolver endpoint %q has no host", parsed.String())
		}

		return &Client{
			credential: credential,
			httpClient: &http.Client{
				Transport: &http.Transport{
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
				},
			},
			url:     strings.TrimRight(parsed.String(), "/") + resolvePath,
			timeout: timeout,
		}, nil

	default:
		return nil, fmt.Errorf(`unsupported secret resolver endpoint %q: expected "unix://<path>", "http://<host>" or "https://<host>"`, redactEndpoint(endpoint))
	}
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
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("failed to build the secret resolver request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	// Set here rather than left to net/http, which derives the same header from userinfo
	// in the URL - at the price of keeping the credential in c.url, where every transport
	// error prints it. A token passed in the username position has no password, which
	// basic auth encodes as an empty one. See Client.credential.
	if c.credential != nil {
		password, _ := c.credential.Password()

		req.SetBasicAuth(c.credential.Username(), password)
	}

	log.Debug().Int("references", len(unique)).Msg("Resolving secret references")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to reach the secret resolver: %w", err)
	}
	defer resp.Body.Close()

	// One byte past the cap, so a body of exactly maxResponseBytes - a legitimate
	// response - is read in full and is still distinguishable from a truncated one.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read the secret resolver response: %w", err)
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
		if err := json.Unmarshal(body, &parsed); err == nil && parsed.Error != "" {
			return nil, fmt.Errorf("secret resolver returned status %d: %s", resp.StatusCode, truncateResolverError(parsed.Error))
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
			Str("content_type", truncateContentType(resp.Header.Get("Content-Type"))).
			Int("body_bytes", len(body)).
			Msg("Secret resolver returned a response that could not be decoded, its content is withheld because it may contain resolved values")

		return nil, errors.New("failed to decode the secret resolver response, see the Portainer server log")
	}

	// Defence in depth against a second implementation of the wire contract rather
	// than a check the current resolver can trip: it never puts an error field in a
	// 200. An implementation that did, with no values, would otherwise be reported as
	// "no value for reference X", hiding the reason - a locked vault, say.
	if parsed.Error != "" {
		return nil, fmt.Errorf("secret resolver returned an error: %s", truncateResolverError(parsed.Error))
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
			return nil, fmt.Errorf("secret resolver returned no value for reference %q", ref)
		}

		values[ref] = value
	}

	return values, nil
}

// truncateResolverError bounds the resolver's error field before it is echoed into
// an error of ours. See maxResolverErrorBytes for why the bound exists.
func truncateResolverError(text string) string {
	return truncate(text, maxResolverErrorBytes)
}

// truncateContentType bounds the resolver's Content-Type header before it is logged.
// See maxContentTypeBytes for why it is bounded separately from the error field.
func truncateContentType(text string) string {
	return truncate(text, maxContentTypeBytes)
}

// truncate cuts text to at most limit bytes, marking it when anything was dropped.
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
