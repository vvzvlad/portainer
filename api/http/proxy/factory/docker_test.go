package factory

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"testing"

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
