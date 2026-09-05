package secretresolver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// socketPath returns a path for a unix socket. It prefers t.TempDir(), but the
// sockaddr_un path is capped at about 104 bytes and Go derives the temp directory
// name from the full test name, so a long test name would overflow it.
func socketPath(t *testing.T) string {
	t.Helper()

	if p := filepath.Join(t.TempDir(), "r.sock"); len(p) < 100 {
		return p
	}

	dir, err := os.MkdirTemp("", "sr")
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(dir) })

	return filepath.Join(dir, "r.sock")
}

// startResolver serves handler over a unix socket and returns its endpoint.
func startResolver(t *testing.T, handler http.Handler) string {
	t.Helper()

	p := socketPath(t)

	listener, err := net.Listen("unix", p)
	require.NoError(t, err)

	server := &http.Server{Handler: handler}
	go server.Serve(listener)

	t.Cleanup(func() { server.Close() })

	return "unix://" + p
}

// serveRawResponse answers every request with response, byte for byte, and returns the
// endpoint.
//
// It exists for the one thing an httptest server cannot do: net/http always writes the
// canonical reason phrase for the code it is given, so a planted phrase can only come
// from a listener that writes the status line itself.
func serveRawResponse(t *testing.T, response string) string {
	t.Helper()

	p := socketPath(t)

	listener, err := net.Listen("unix", p)
	require.NoError(t, err)

	t.Cleanup(func() { listener.Close() })

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}

			go func() {
				defer conn.Close()

				// The request is read to completion before the answer goes out, so the
				// client is never writing into a connection that has already been closed.
				if req, err := http.ReadRequest(bufio.NewReader(conn)); err == nil {
					io.Copy(io.Discard, req.Body)
				}

				conn.Write([]byte(response))
			}()
		}
	}()

	return "unix://" + p
}

func newTestClient(t *testing.T, endpoint string) *Client {
	t.Helper()

	client, err := New(endpoint, time.Second)
	require.NoError(t, err)

	return client
}

// withUserinfo splices a credential into an endpoint, which is the only way this protocol
// offers to authenticate to a remote resolver.
func withUserinfo(endpoint, userinfo string) string {
	scheme, rest, _ := strings.Cut(endpoint, "://")

	return scheme + "://" + userinfo + "@" + rest
}

// captureLog points the global zerolog logger at a buffer for the duration of the test.
//
// A test that uses it must not call t.Parallel. That is safe: the package's parallel tests
// resume only once every sequential top-level test has finished.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()

	var buf bytes.Buffer

	previous := log.Logger
	log.Logger = zerolog.New(&buf)

	t.Cleanup(func() { log.Logger = previous })

	return &buf
}

func TestIsReference(t *testing.T) {
	t.Parallel()

	assert.True(t, IsReference("secret:vw:stack/nebula/arcextension/ADMIN_TOKEN"))
	assert.False(t, IsReference("hunter2"))
	assert.False(t, IsReference(""))
	assert.False(t, IsReference("notsecret:x"))

	// The escaped marker is a literal, not a reference: without this a stack variable
	// whose value legitimately begins with "secret:" would be undeployable on this fork.
	assert.False(t, IsReference("secret::foo"))
	assert.False(t, IsReference("secret::"))
	assert.True(t, IsReference("secret:vw:x"))
}

func TestUnescape(t *testing.T) {
	t.Parallel()

	// One colon is removed, and only from the marker: the escape is the doubling.
	assert.Equal(t, "secret:foo", Unescape("secret::foo"))
	assert.Equal(t, "secret::x", Unescape("secret:::x"))
	assert.Equal(t, "secret:", Unescape("secret::"))

	// A reference and an ordinary value are both left exactly as they are.
	assert.Equal(t, "secret:vw:x", Unescape("secret:vw:x"))
	assert.Equal(t, "hunter2", Unescape("hunter2"))
	assert.Equal(t, "", Unescape(""))
	assert.Equal(t, "postgres://user@host/db", Unescape("postgres://user@host/db"))
}

func TestFetch(t *testing.T) {
	t.Parallel()

	t.Run("returns a value per reference", func(t *testing.T) {
		t.Parallel()

		endpoint := startResolver(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, http.MethodPost, r.Method)
			assert.Equal(t, resolvePath, r.URL.Path)
			assert.Equal(t, "application/json", r.Header.Get("Content-Type"))

			var req resolveRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			assert.Equal(t, protocolVersion, req.Version)
			// The whole reference string is forwarded, prefix included.
			assert.Equal(t, []string{"secret:vw:a", "secret:vw:b"}, req.Refs)

			json.NewEncoder(w).Encode(resolveResponse{Values: map[string]string{
				"secret:vw:a": "value-a",
				"secret:vw:b": "value-b",
			}})
		}))

		values, err := newTestClient(t, endpoint).Fetch(t.Context(), []string{"secret:vw:a", "secret:vw:b"})
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"secret:vw:a": "value-a", "secret:vw:b": "value-b"}, values)
	})

	t.Run("deduplicates references", func(t *testing.T) {
		t.Parallel()

		var got []string

		endpoint := startResolver(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req resolveRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			got = req.Refs

			json.NewEncoder(w).Encode(resolveResponse{Values: map[string]string{"secret:vw:a": "value-a", "secret:vw:b": "value-b"}})
		}))

		refs := []string{"secret:vw:a", "secret:vw:b", "secret:vw:a", "secret:vw:a"}

		values, err := newTestClient(t, endpoint).Fetch(t.Context(), refs)
		require.NoError(t, err)
		assert.Equal(t, []string{"secret:vw:a", "secret:vw:b"}, got)
		assert.Len(t, values, 2)
	})

	t.Run("fails on a missing reference", func(t *testing.T) {
		t.Parallel()

		endpoint := startResolver(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(resolveResponse{Values: map[string]string{"secret:vw:a": "value-a"}})
		}))

		// A partial result is never returned: it would deploy a service with an empty
		// credential instead of failing the deploy.
		values, err := newTestClient(t, endpoint).Fetch(t.Context(), []string{"secret:vw:a", "secret:vw:missing"})
		require.Error(t, err)
		assert.Nil(t, values)
		assert.Contains(t, err.Error(), "secret:vw:missing")
	})

	t.Run("fails on an empty value", func(t *testing.T) {
		t.Parallel()

		endpoint := startResolver(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(resolveResponse{Values: map[string]string{"secret:vw:a": ""}})
		}))

		// An empty value is a missing value. A bare ${VAR} in a compose body would
		// otherwise start a service on a blank password with only a warning.
		values, err := newTestClient(t, endpoint).Fetch(t.Context(), []string{"secret:vw:a"})
		require.Error(t, err)
		assert.Nil(t, values)
		assert.Contains(t, err.Error(), "secret:vw:a")
	})

	t.Run("reports an error field on a 200", func(t *testing.T) {
		t.Parallel()

		endpoint := startResolver(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(resolveResponse{Error: "vault is locked"})
		}))

		// Without this the real reason would be hidden behind "no value for reference".
		_, err := newTestClient(t, endpoint).Fetch(t.Context(), []string{"secret:vw:a"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "vault is locked")
	})

	t.Run("reports a non-200 with its error body", func(t *testing.T) {
		t.Parallel()

		endpoint := startResolver(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
			json.NewEncoder(w).Encode(resolveResponse{Error: "vault sync failed"})
		}))

		_, err := newTestClient(t, endpoint).Fetch(t.Context(), []string{"secret:vw:a"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "502")
		assert.Contains(t, err.Error(), "vault sync failed")
	})

	t.Run("keeps a malformed body out of the returned error", func(t *testing.T) {
		t.Parallel()

		// The decoder quotes what it choked on: segmentio/encoding formats a syntax
		// error as "json: <message>: <first 32 bytes of the buffer>". The buffer is the
		// response body, so those 32 bytes reach past the reference and into the value -
		// and the error would be persisted as the stack's deployment status message.
		const secretValue = "Sup3rSecretValue"

		// The prefix before the value is 26 bytes, so this much of the value falls
		// inside the decoder's 32-byte window.
		exposedPrefix := secretValue[:6]

		body := `{"values":{"secret:vw:a":"` + secretValue

		endpoint := startResolver(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(body))
		}))

		_, err := newTestClient(t, endpoint).Fetch(t.Context(), []string{"secret:vw:a"})
		require.Error(t, err)
		assert.NotContains(t, err.Error(), secretValue)
		assert.NotContains(t, err.Error(), exposedPrefix)
		assert.Contains(t, err.Error(), "decode")
	})

	t.Run("truncates the resolver error field", func(t *testing.T) {
		t.Parallel()

		// The field is stored as the stack's deployment status message, so how much of
		// somebody else's text ends up in Portainer's database is bounded here.
		long := strings.Repeat("z", maxResolverErrorBytes*3)

		endpoint := startResolver(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(resolveResponse{Error: long})
		}))

		_, err := newTestClient(t, endpoint).Fetch(t.Context(), []string{"secret:vw:a"})
		require.Error(t, err)
		assert.NotContains(t, err.Error(), long)
		assert.Contains(t, err.Error(), truncationMarker)
		assert.Less(t, len(err.Error()), maxResolverErrorBytes*2)
	})

	t.Run("truncates the resolver error field of a non-200 too", func(t *testing.T) {
		t.Parallel()

		long := strings.Repeat("z", maxResolverErrorBytes*3)

		endpoint := startResolver(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
			json.NewEncoder(w).Encode(resolveResponse{Error: long})
		}))

		_, err := newTestClient(t, endpoint).Fetch(t.Context(), []string{"secret:vw:a"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "502")
		assert.NotContains(t, err.Error(), long)
		assert.Contains(t, err.Error(), truncationMarker)
	})

	t.Run("reports a non-200 without a body", func(t *testing.T) {
		t.Parallel()

		endpoint := startResolver(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))

		_, err := newTestClient(t, endpoint).Fetch(t.Context(), []string{"secret:vw:a"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "500")
	})

	t.Run("keeps the status line reason phrase out of the returned error", func(t *testing.T) {
		t.Parallel()

		// resp.Status is "<code> <reason phrase>" as the server wrote it, and the phrase
		// is the resolver's own text - capped only by net/http's 10 MiB header limit, and
		// settable by anyone on the path of an http:// endpoint. Echoed, it would be a
		// second unbounded channel of resolver text into an error Portainer persists as
		// the stack's deployment status message, defeating maxResolverErrorBytes through
		// the field next door. The subtests above cannot catch this: an httptest server
		// only ever writes the canonical phrase for the code it is given.
		planted := "PLANTEDREASON" + strings.Repeat("q", 2048)

		body := `{"error":"vault sync failed"}`

		tests := []struct {
			name     string
			response string
			// contains is the diagnostic that must survive, so the assertions cannot be
			// satisfied by a message that says nothing at all.
			contains string
		}{
			{
				name:     "with an error field",
				response: "HTTP/1.1 502 " + planted + "\r\nContent-Type: application/json\r\nContent-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n" + body,
				contains: "vault sync failed",
			},
			{
				name:     "without a body",
				response: "HTTP/1.1 500 " + planted + "\r\nContent-Length: 0\r\n\r\n",
				contains: "500",
			},
		}

		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				t.Parallel()

				endpoint := serveRawResponse(t, test.response)

				_, err := newTestClient(t, endpoint).Fetch(t.Context(), []string{"secret:vw:a"})
				require.Error(t, err)
				assert.NotContains(t, err.Error(), "PLANTEDREASON")
				assert.NotContains(t, err.Error(), "qqq")
				assert.Contains(t, err.Error(), test.contains)
			})
		}
	})

	t.Run("applies its own timeout", func(t *testing.T) {
		t.Parallel()

		release := make(chan struct{})
		t.Cleanup(func() { close(release) })

		endpoint := startResolver(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			<-release
		}))

		client, err := New(endpoint, 50*time.Millisecond)
		require.NoError(t, err)

		// The deploy context is deliberately unbounded here: a wedged resolver must
		// still fail in milliseconds rather than inherit the deploy's deadline.
		start := time.Now()
		_, err = client.Fetch(context.Background(), []string{"secret:vw:a"})
		require.ErrorIs(t, err, context.DeadlineExceeded)

		elapsed := time.Since(start)
		assert.GreaterOrEqual(t, elapsed, 50*time.Millisecond)
		assert.Less(t, elapsed, 5*time.Second)
	})

	t.Run("works over http", func(t *testing.T) {
		t.Parallel()

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, resolvePath, r.URL.Path)

			json.NewEncoder(w).Encode(resolveResponse{Values: map[string]string{"secret:vw:a": "value-a"}})
		}))
		t.Cleanup(server.Close)

		values, err := newTestClient(t, server.URL).Fetch(t.Context(), []string{"secret:vw:a"})
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"secret:vw:a": "value-a"}, values)
	})

	t.Run("caps the response size", func(t *testing.T) {
		t.Parallel()

		chunk := bytes.Repeat([]byte("a"), 64*1024)

		endpoint := startResolver(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"values":{"secret:vw:a":"`))

			// Comfortably past the cap, so it is the limit reader that ends the read.
			for range maxResponseBytes/len(chunk) + 4 {
				w.Write(chunk)
			}
		}))

		// The cap has to be named: a silently truncated body would otherwise fail to
		// parse with the same error a corrupt one gives, and be undiagnosable.
		_, err := newTestClient(t, endpoint).Fetch(t.Context(), []string{"secret:vw:a"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "exceeds")
		assert.Contains(t, err.Error(), strconv.Itoa(maxResponseBytes))
	})

	t.Run("accepts a response of exactly the cap", func(t *testing.T) {
		t.Parallel()

		// The boundary belongs to the legitimate side: a body of exactly maxResponseBytes
		// is within the limit, not over it.
		prefix := `{"values":{"secret:vw:a":"`
		suffix := `"}}`
		value := strings.Repeat("a", maxResponseBytes-len(prefix)-len(suffix))
		body := prefix + value + suffix
		require.Len(t, body, maxResponseBytes)

		endpoint := startResolver(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(body))
		}))

		values, err := newTestClient(t, endpoint).Fetch(t.Context(), []string{"secret:vw:a"})
		require.NoError(t, err)
		assert.Equal(t, value, values["secret:vw:a"])
	})

	t.Run("does no I/O without references", func(t *testing.T) {
		t.Parallel()

		endpoint := startResolver(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Error("the resolver must not be called without references")
		}))

		values, err := newTestClient(t, endpoint).Fetch(t.Context(), nil)
		require.NoError(t, err)
		assert.Empty(t, values)
	})

	t.Run("a nil client is not configured", func(t *testing.T) {
		t.Parallel()

		var client *Client

		_, err := client.Fetch(t.Context(), []string{"secret:vw:a"})
		require.Error(t, err)
	})

	t.Run("sends the endpoint credential as basic auth", func(t *testing.T) {
		t.Parallel()

		// The credential is kept out of the URL so that no transport error can print it,
		// which means the header net/http would have derived from the userinfo has to be
		// built by hand. Without this a resolver behind authentication would start refusing
		// every deploy, and nothing else in the suite would notice.
		type credential struct {
			username string
			password string
			ok       bool
		}

		tests := []struct {
			name     string
			userinfo string
			want     credential
		}{
			{
				name:     "a token in the username",
				userinfo: "Zt9mLr5Vb2nHd8",
				want:     credential{username: "Zt9mLr5Vb2nHd8", ok: true},
			},
			{
				name:     "a username and a password",
				userinfo: "portainer:Zt9mLr5Vb2nHd8",
				want:     credential{username: "portainer", password: "Zt9mLr5Vb2nHd8", ok: true},
			},
		}

		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				t.Parallel()

				// A channel and not a plain variable: the handler runs on its own goroutine,
				// and a response body read is not an ordering the race detector knows about.
				got := make(chan credential, 1)

				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					username, password, ok := r.BasicAuth()
					got <- credential{username: username, password: password, ok: ok}

					json.NewEncoder(w).Encode(resolveResponse{Values: map[string]string{"secret:vw:a": "value-a"}})
				}))
				t.Cleanup(server.Close)

				endpoint := withUserinfo(server.URL, test.userinfo)

				_, err := newTestClient(t, endpoint).Fetch(t.Context(), []string{"secret:vw:a"})
				require.NoError(t, err)

				assert.Equal(t, test.want, <-got)
			})
		}
	})

	t.Run("keeps the endpoint credential out of a transport error", func(t *testing.T) {
		t.Parallel()

		// The most common failure this feature has - a resolver that is down, unreachable
		// or wedged - and the one that publishes. http.Client.Do fails with a *url.Error
		// whose URL is stripPassword(req.URL), and that masks the password only: a token in
		// the username position, which is the form the endpoint documentation offers, comes
		// back whole. The error is wrapped into the stack's deployment status message, which
		// Portainer persists and StackInspect serves, and it repeats on every retry.
		const token = "Zt9mLr5Vb2nHd8"

		wedged := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Released by the client's own deadline closing the connection; the timer is
			// only a backstop so a failing case cannot wedge the suite.
			select {
			case <-r.Context().Done():
			case <-time.After(2 * time.Second):
			}
		}))
		t.Cleanup(wedged.Close)

		tests := []struct {
			name     string
			endpoint string
			timeout  time.Duration
			// contains is the diagnostic that must survive, so the assertion cannot be
			// satisfied by a message that says nothing at all.
			contains string
		}{
			{
				name: "unreachable, with a token in the username",
				// Nothing listens on port 1, so the dial is refused rather than hung.
				endpoint: "http://" + token + "@127.0.0.1:1",
				timeout:  5 * time.Second,
				contains: "127.0.0.1:1",
			},
			{
				name:     "unreachable, with a username and a password",
				endpoint: "http://portainer:" + token + "@127.0.0.1:1",
				timeout:  5 * time.Second,
				contains: "127.0.0.1:1",
			},
			{
				name:     "no answer in time, with a token in the username",
				endpoint: withUserinfo(wedged.URL, token),
				timeout:  50 * time.Millisecond,
				contains: "deadline exceeded",
			},
			{
				name:     "no answer in time, with a username and a password",
				endpoint: withUserinfo(wedged.URL, "portainer:"+token),
				timeout:  50 * time.Millisecond,
				contains: "deadline exceeded",
			},
		}

		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				t.Parallel()

				client, err := New(test.endpoint, test.timeout)
				require.NoError(t, err)

				_, err = client.Fetch(t.Context(), []string{"secret:vw:a"})
				require.Error(t, err)

				assert.NotContains(t, err.Error(), token)
				assert.Contains(t, err.Error(), test.contains)
			})
		}
	})
}

// TestFetchBoundsTheLoggedContentType pins the one bound in this package that nothing else
// covers: replacing the truncation with the raw header left the whole suite green.
//
// The header is resolver-written text, capped only by net/http's 10 MiB response header
// limit, and the log is a lesser channel than the database but not a free one.
func TestFetchBoundsTheLoggedContentType(t *testing.T) {
	// The global logger is swapped, so this test does not run in parallel.
	buf := captureLog(t)

	long := strings.Repeat("z", maxContentTypeBytes*3)

	endpoint := startResolver(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", long)
		w.Write([]byte("this is not the agreed JSON"))
	}))

	_, err := newTestClient(t, endpoint).Fetch(t.Context(), []string{"secret:vw:a"})
	require.Error(t, err)

	output := buf.String()

	// The line is the only diagnosis an undecodable response leaves, so it has to be there
	// and it has to keep saying what the resolver answered with.
	require.Contains(t, output, "content_type")
	require.Contains(t, output, "zzz")

	assert.NotContains(t, output, long)
	assert.Contains(t, output, truncationMarker)
}

func TestNew(t *testing.T) {
	t.Parallel()

	t.Run("rejects an unsupported scheme", func(t *testing.T) {
		t.Parallel()

		_, err := New("tcp://localhost:9000", time.Second)
		require.Error(t, err)
	})

	t.Run("rejects an empty endpoint", func(t *testing.T) {
		t.Parallel()

		_, err := New("", time.Second)
		require.Error(t, err)
	})

	t.Run("rejects an empty socket path", func(t *testing.T) {
		t.Parallel()

		_, err := New("unix://", time.Second)
		require.Error(t, err)
	})

	t.Run("rejects an http endpoint without a host", func(t *testing.T) {
		t.Parallel()

		// A prefix check alone accepts these, and the mistake would then only surface at
		// the first deploy of a migrated stack as "no Host in request URL".
		for _, endpoint := range []string{"http://", "https://", "http:///v1"} {
			_, err := New(endpoint, time.Second)
			require.Error(t, err, endpoint)
			assert.Contains(t, err.Error(), "no host")
		}
	})

	t.Run("rejects a non-positive timeout", func(t *testing.T) {
		t.Parallel()

		_, err := New("unix:///run/resolver.sock", 0)
		require.Error(t, err)
	})

	t.Run("keeps the endpoint path for http", func(t *testing.T) {
		t.Parallel()

		client, err := New("http://resolver:9100/", time.Second)
		require.NoError(t, err)
		assert.Equal(t, "http://resolver:9100"+resolvePath, client.url)
	})

	t.Run("keeps the endpoint credential out of the client URL", func(t *testing.T) {
		t.Parallel()

		const token = "Zt9mLr5Vb2nHd8"

		// url is what net/http prints in every transport error, so the credential is held
		// beside it and applied per request instead. Both forms: the password is the one
		// net/http masks on its own, the token in the username position is the one it does
		// not - and the latter is the form this protocol actually offers.
		for _, userinfo := range []string{token, "portainer:" + token} {
			client, err := New("http://"+userinfo+"@resolver:9100", time.Second)
			require.NoError(t, err)

			assert.Equal(t, "http://resolver:9100"+resolvePath, client.url)

			// Stripped, not dropped: it is still sent, as a header built in Fetch.
			require.NotNil(t, client.credential)
			assert.Contains(t, client.credential.String(), token)
		}
	})

	t.Run("keeps the endpoint credential out of its errors", func(t *testing.T) {
		t.Parallel()

		const token = "Zt9mLr5Vb2nHd8"

		// New's error is kept as configErr and returned by every Fetch, so it reaches the
		// stack's deployment status message on every deploy rather than once. url.Parse is
		// the trap: its *url.Error prints the URL it was handed in full, with no masking of
		// any kind, so wrapping it would publish the userinfo whatever the format verb says.
		tests := []struct {
			name     string
			endpoint string
		}{
			{name: "unparsable, token in the username", endpoint: "http://" + token + "@host:notaport"},
			{name: "unparsable, username and password", endpoint: "http://portainer:" + token + "@host:notaport"},
			{name: "no host, token in the username", endpoint: "http://" + token + "@"},
			{name: "no host, username and password", endpoint: "http://portainer:" + token + "@"},
			{name: "unsupported scheme, token in the username", endpoint: "tcp://" + token + "@resolver:9100"},
			{name: "unsupported scheme, username and password", endpoint: "tcp://portainer:" + token + "@resolver:9100"},
		}

		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				t.Parallel()

				_, err := New(test.endpoint, time.Second)
				require.Error(t, err)
				assert.NotContains(t, err.Error(), token)
			})
		}
	})

	t.Run("never consults the proxy environment", func(t *testing.T) {
		t.Parallel()

		// http.DefaultTransport - what an &http.Client{} with no transport of its own
		// uses - proxies through ProxyFromEnvironment. The resolver's response body is
		// the secret values, so under a corporate HTTP_PROXY they would be relayed
		// through it in clear text. Both endpoint forms must build their own transport.
		for _, endpoint := range []string{"http://resolver:9100", "https://resolver:9100", "unix:///run/resolver.sock"} {
			client, err := New(endpoint, time.Second)
			require.NoError(t, err, endpoint)

			transport, ok := client.httpClient.Transport.(*http.Transport)
			require.True(t, ok, endpoint)
			assert.Nil(t, transport.Proxy, endpoint)
		}
	})
}

func TestTruncateResolverError(t *testing.T) {
	t.Parallel()

	t.Run("leaves a message within the bound alone", func(t *testing.T) {
		t.Parallel()

		text := strings.Repeat("z", maxResolverErrorBytes)

		assert.Equal(t, text, truncateResolverError(text))
	})

	t.Run("marks a truncated message", func(t *testing.T) {
		t.Parallel()

		truncated := truncateResolverError(strings.Repeat("z", maxResolverErrorBytes+1))

		assert.Equal(t, strings.Repeat("z", maxResolverErrorBytes)+truncationMarker, truncated)
	})

	t.Run("cuts on a rune boundary", func(t *testing.T) {
		t.Parallel()

		// The cut offset falls in the middle of the two-byte rune, which a plain slice
		// would split and render as a replacement character.
		text := strings.Repeat("z", maxResolverErrorBytes-1) + "é" + "tail"

		truncated := truncateResolverError(text)

		assert.True(t, utf8.ValidString(truncated))
		assert.Equal(t, strings.Repeat("z", maxResolverErrorBytes-1)+truncationMarker, truncated)
	})
}

// The env var tests cannot run in parallel: t.Setenv forbids it.
func TestFromEnv(t *testing.T) {
	t.Run("returns nil when unset", func(t *testing.T) {
		t.Setenv(EndpointEnvVar, "")

		assert.Nil(t, FromEnv())
	})

	t.Run("uses the default timeout", func(t *testing.T) {
		t.Setenv(EndpointEnvVar, "unix:///run/secret-resolver/resolver.sock")

		client := FromEnv()
		require.NotNil(t, client)
		require.NoError(t, client.configErr)
		assert.Equal(t, DefaultTimeout, client.timeout)
	})

	t.Run("honours an explicit timeout", func(t *testing.T) {
		t.Setenv(EndpointEnvVar, "http://resolver:9100")
		t.Setenv(TimeoutEnvVar, "3s")

		client := FromEnv()
		require.NotNil(t, client)
		require.NoError(t, client.configErr)
		assert.Equal(t, 3*time.Second, client.timeout)
	})

	t.Run("fails every fetch on a bad timeout", func(t *testing.T) {
		t.Setenv(EndpointEnvVar, "unix:///run/secret-resolver/resolver.sock")
		t.Setenv(TimeoutEnvVar, "fifteen seconds")

		// A broken configuration must not degrade into "no resolver configured",
		// which would let a stack deploy its references verbatim.
		client := FromEnv()
		require.NotNil(t, client)

		_, err := client.Fetch(t.Context(), []string{"secret:vw:a"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), TimeoutEnvVar)
	})

	t.Run("fails every fetch on a bad endpoint", func(t *testing.T) {
		t.Setenv(EndpointEnvVar, "tcp://resolver:9100")

		client := FromEnv()
		require.NotNil(t, client)

		_, err := client.Fetch(t.Context(), []string{"secret:vw:a"})
		require.Error(t, err)
	})

	t.Run("keeps endpoint credentials out of the log", func(t *testing.T) {
		// Userinfo is the only way this protocol offers to authenticate to a remote
		// resolver, and net/http itself treats it as secret, stripping the password from
		// its own url.Error. New keeps it out of the client's URL, so these two startup
		// lines are the last place the endpoint as configured is in hand at all.
		const token = "Zt9mLr5Vb2nHd8"

		t.Setenv(EndpointEnvVar, "http://portainer:"+token+"@resolver.example:9100")

		buf := captureLog(t)

		require.NotNil(t, FromEnv())

		output := buf.String()

		// Plain HTTP to a non-loopback host, so both lines fire and both are covered.
		require.Contains(t, output, "resolver.example:9100")
		require.Contains(t, output, "clear text")

		assert.NotContains(t, output, token)
		assert.NotContains(t, output, "portainer:")
	})

	t.Run("keeps endpoint credentials out of a configuration error", func(t *testing.T) {
		// The startup lines above are logged once; this error is kept as configErr and
		// returned by every Fetch, so one misconfigured endpoint publishes on every deploy
		// of every stack that uses references - into the database, not just the log.
		const token = "Zt9mLr5Vb2nHd8"

		t.Setenv(EndpointEnvVar, "tcp://portainer:"+token+"@resolver.example:9100")

		buf := captureLog(t)

		client := FromEnv()
		require.NotNil(t, client)

		_, err := client.Fetch(t.Context(), []string{"secret:vw:a"})
		require.Error(t, err)

		assert.NotContains(t, err.Error(), token)
		assert.NotContains(t, buf.String(), token)

		// The diagnosis survives: the operator is still told which endpoint is wrong.
		assert.Contains(t, err.Error(), "resolver.example:9100")
	})
}
