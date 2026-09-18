package factory

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	portainer "github.com/portainer/portainer/api"
	"github.com/portainer/portainer/api/http/proxy/factory/docker"

	"github.com/stretchr/testify/require"
)

// chunkedReader yields a fixed list of chunks across successive Read calls. A
// chunk larger than the caller's buffer is returned in pieces; once the LAST
// chunk has been fully consumed, the final Read returns its bytes together with
// io.EOF in the SAME call — the (n>0, io.EOF) case streamResponse must handle so
// the tail of a streamed Docker response is not dropped.
type chunkedReader struct {
	chunks [][]byte
	i      int
}

func (r *chunkedReader) Read(p []byte) (int, error) {
	if r.i >= len(r.chunks) {
		return 0, io.EOF
	}
	c := r.chunks[r.i]
	n := copy(p, c)
	if n < len(c) {
		// Buffer too small for the whole chunk: keep the remainder for the next
		// Read and report no error yet.
		r.chunks[r.i] = c[n:]
		return n, nil
	}
	r.i++
	if r.i >= len(r.chunks) {
		// Last chunk fully delivered: hand back the data AND io.EOF at once.
		return n, io.EOF
	}
	return n, nil
}

// countingFlushWriter is a minimal http.ResponseWriter (with http.Flusher) that
// accumulates the body and counts Flush() calls.
type countingFlushWriter struct {
	buf        bytes.Buffer
	header     http.Header
	flushCount int
}

func (w *countingFlushWriter) Header() http.Header {
	if w.header == nil {
		w.header = http.Header{}
	}
	return w.header
}
func (w *countingFlushWriter) Write(p []byte) (int, error) { return w.buf.Write(p) }
func (w *countingFlushWriter) WriteHeader(int)             {}
func (w *countingFlushWriter) Flush()                      { w.flushCount++ }

// errOnWriteWriter fails on the first Write and does NOT implement http.Flusher,
// so it also exercises streamResponse's nil-flusher fallback path.
type errOnWriteWriter struct {
	header http.Header
	calls  int
}

func (w *errOnWriteWriter) Header() http.Header {
	if w.header == nil {
		w.header = http.Header{}
	}
	return w.header
}
func (w *errOnWriteWriter) Write(p []byte) (int, error) {
	w.calls++
	return 0, errors.New("write failed")
}
func (w *errOnWriteWriter) WriteHeader(int) {}

func TestStreamResponse_DeliversFullBodyAndFlushesPerChunk(t *testing.T) {
	t.Parallel()
	// A body larger than streamResponse's 32KB internal buffer, made of three
	// distinct regions so any loss/duplication/reordering is detectable. The
	// chunk boundaries do not align with the 32KB buffer, exercising both the
	// partial-read path and the final (n>0, io.EOF) read.
	regionA := bytes.Repeat([]byte("A"), 40000)
	regionB := bytes.Repeat([]byte("B"), 20000)
	regionC := bytes.Repeat([]byte("C"), 5000)
	want := bytes.Join([][]byte{regionA, regionB, regionC}, nil)

	r := &chunkedReader{chunks: [][]byte{
		append([]byte(nil), regionA...),
		append([]byte(nil), regionB...),
		append([]byte(nil), regionC...),
	}}
	w := &countingFlushWriter{}

	streamResponse(w, r)

	require.Equal(t, want, w.buf.Bytes(), "streamed body must equal the input exactly (no loss/duplication)")
	require.Greater(t, w.flushCount, 1, "must flush more than once for a multi-chunk body (the whole point of the change)")
}

func TestStreamResponse_StopsOnWriteErrorWithoutPanic(t *testing.T) {
	t.Parallel()
	r := &chunkedReader{chunks: [][]byte{[]byte("hello"), []byte("world")}}
	w := &errOnWriteWriter{}

	require.NotPanics(t, func() { streamResponse(w, r) })
	require.Equal(t, 1, w.calls, "must break the loop on the first write error, not keep writing")
}

// newTestDockerLocalProxy builds a dockerLocalProxy whose Docker transport dials
// backendAddr (a fake daemon) instead of a real Unix socket. The environment type
// is a plain Docker one, so ProxyDockerRequest skips the agent request-signing
// branch and no signature service is needed.
func newTestDockerLocalProxy(t *testing.T, backendAddr string) *dockerLocalProxy {
	t.Helper()

	// The ssrf ruleguard forbids a bare *http.Transport because production code must
	// route outbound requests through ssrf.NewTransport. This one never leaves the
	// test: it dials the fake daemon this test started, standing in for the Unix
	// socket of the real one.
	httpTransport := &http.Transport{ //nolint:gocritic
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", backendAddr)
		},
	}

	transport, err := docker.NewTransport(
		&docker.TransportParameters{Endpoint: &portainer.Endpoint{Type: portainer.DockerEnvironment}},
		httpTransport,
		nil,
		nil,
	)
	require.NoError(t, err)

	return &dockerLocalProxy{transport: transport}
}

func TestDockerLocalProxy_SilentStreamDeliversHeadersBeforeAnyBody(t *testing.T) {
	t.Parallel()

	// Fake daemon answering like /events on an idle host: chunked (no
	// Content-Length) and not a single body byte until released.
	release := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Stream-Marker", "events")
		w.WriteHeader(http.StatusTeapot)
		w.(http.Flusher).Flush()
		<-release
	}))
	defer backend.Close()

	// /events has no entry in prefixProxyFuncMap and is not an admin-only route,
	// so the request goes straight to the transport and out to the backend.
	front := httptest.NewServer(newTestDockerLocalProxy(t, backend.Listener.Addr().String()))
	defer front.Close()

	// Deferred calls run LIFO, so the backend handler is released before either
	// server is closed - otherwise Close would wait on the blocked handler.
	defer close(release)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, front.URL+"/events", nil)
	require.NoError(t, err)

	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err, "status and headers must reach the client before any body byte is produced")

	defer func() {
		require.NoError(t, res.Body.Close())
	}()

	require.Equal(t, http.StatusTeapot, res.StatusCode)
	require.Equal(t, "events", res.Header.Get("X-Stream-Marker"))
}

func TestDockerLocalProxy_BufferedResponseKeepsContentLength(t *testing.T) {
	t.Parallel()

	// End-to-end sentinel for an ordinary response: it must reach the client byte
	// for byte with its framing intact. This one cannot tell whether the
	// ContentLength gate is present - ServeHTTP copies the upstream Content-Length
	// header verbatim, so net/http on this side never has to compute one and an
	// early flush would change nothing. TestDockerLocalProxy_FlushIsGatedOnContentLength
	// is what actually pins the gate.
	want := []byte(`{"ID":"NYGS:LWGE","Containers":3}`)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write(want); err != nil {
			t.Error(err)
		}
	}))
	defer backend.Close()

	front := httptest.NewServer(newTestDockerLocalProxy(t, backend.Listener.Addr().String()))
	defer front.Close()

	// /info is absent from prefixProxyFuncMap and is not an admin-only route, so
	// the request reaches the backend unmodified.
	res, err := http.Get(front.URL + "/info")
	require.NoError(t, err)

	defer func() {
		require.NoError(t, res.Body.Close())
	}()

	got, err := io.ReadAll(res.Body)
	require.NoError(t, err)

	require.Equal(t, http.StatusOK, res.StatusCode)
	require.Equal(t, want, got, "a buffered response must pass through byte for byte")
	require.Equal(t, int64(len(want)), res.ContentLength, "Content-Length must survive the proxy")
	require.Empty(t, res.TransferEncoding, "a buffered response must not be moved to chunked encoding")
}

// orderedFlushWriter is a minimal http.ResponseWriter (with http.Flusher) that
// records the order of Write and Flush calls, so a test can tell whether the
// headers were flushed before the first body byte or only after it.
type orderedFlushWriter struct {
	header http.Header
	ops    []string
}

func (w *orderedFlushWriter) Header() http.Header {
	if w.header == nil {
		w.header = http.Header{}
	}
	return w.header
}

func (w *orderedFlushWriter) Write(p []byte) (int, error) {
	w.ops = append(w.ops, "write")
	return len(p), nil
}

func (w *orderedFlushWriter) WriteHeader(int) {}
func (w *orderedFlushWriter) Flush()          { w.ops = append(w.ops, "flush") }

// TestDockerLocalProxy_FlushIsGatedOnContentLength pins the gate itself, which no
// end-to-end test in this file can do: driving ServeHTTP with a writer that records
// call order shows whether the early flush fired. A streaming response must be
// flushed before its first body byte; a response carrying a Content-Length must not
// be, because fixing its headers early costs it the Content-Length net/http would
// otherwise compute at handlerDone and moves it to chunked for nothing.
func TestDockerLocalProxy_FlushIsGatedOnContentLength(t *testing.T) {
	t.Parallel()

	body := []byte(`{"status":"ok"}`)

	tests := []struct {
		name    string
		chunked bool
		firstOp string
	}{
		{name: "streaming response is flushed before any body byte", chunked: true, firstOp: "flush"},
		{name: "buffered response is not flushed before its body", chunked: false, firstOp: "write"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tt.chunked {
					// Flushing before the first byte is what makes net/http drop
					// Content-Length and switch this response to chunked.
					w.WriteHeader(http.StatusOK)
					w.(http.Flusher).Flush()
				}
				if _, err := w.Write(body); err != nil {
					t.Error(err)
				}
			}))
			defer backend.Close()

			proxy := newTestDockerLocalProxy(t, backend.Listener.Addr().String())

			rec := &orderedFlushWriter{}
			proxy.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/info", nil))

			require.NotEmpty(t, rec.ops, "the proxy must have written or flushed something")
			require.Equal(t, tt.firstOp, rec.ops[0])
		})
	}
}
