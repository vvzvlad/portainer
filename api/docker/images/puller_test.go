package images

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/portainer/portainer/api/internal/testhelpers"

	dockerclient "github.com/docker/docker/client"
	"github.com/stretchr/testify/require"
)

// A progress line of the shape the daemon actually writes, the failure it reports
// IN BAND — inside a response that has already been given a 200, followed by a
// clean end of stream — and bytes that are not the stream anyone expected. The
// second one is the message this package used to throw away; the third is what
// sends the drain down its fallback path.
const (
	progressLine = `{"status":"Downloading","progressDetail":{"current":1024,"total":8192},"progress":"[===>  ] 1kB/8kB","id":"a1b2c3d4"}` + "\n"
	failureLine  = `{"errorDetail":{"message":"failed to register layer: no space left on device"},"error":"failed to register layer: no space left on device"}` + "\n"
	garbageLine  = "%% this is not the stream anyone expected %%\n"

	// A message that stops mid-way: valid JSON as far as it goes and terminated
	// nowhere, which is what a connection dropped in the middle of a line leaves
	// behind.
	partialLine = `{"status":"Downloading","progressDetail":{"current":10485760,`
)

// drainWithGuard runs a drain under a freshly armed idle clock, the way Pull does.
func drainWithGuard(ctx context.Context, progress io.Reader, stall time.Duration, abort context.CancelFunc) error {
	guard := newStallGuard(stall, abort)
	defer guard.stop()

	return drainProgress(ctx, progress, guard)
}

// steadyReader stands in for a HEALTHY pull: it hands out one small progress
// line every interval until it has produced count of them, then ends the stream.
// It is deliberately slow and long-running — the whole point of the drain is that
// a stream like this must be allowed to run for as long as it keeps talking.
//
// It reads the pull's context so a drain that aborts the pull is observed as an
// aborted read rather than as a test that hangs.
type steadyReader struct {
	ctx      context.Context
	interval time.Duration
	left     int
}

func (r *steadyReader) Read(p []byte) (int, error) {
	if r.left <= 0 {
		return 0, io.EOF
	}

	select {
	case <-time.After(r.interval):
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	}

	r.left--

	return copy(p, []byte(`{"status":"Downloading"}`+"\n")), nil
}

// silentReader is what a half-open connection looks like from the reading side:
// the stream is open, nothing is coming, and only the death of the pull's context
// ever unblocks the read.
type silentReader struct {
	ctx context.Context
}

func (r *silentReader) Read([]byte) (int, error) {
	<-r.ctx.Done()

	return 0, r.ctx.Err()
}

// emptyReader is a stream that keeps ANSWERING without ever saying anything: every
// read returns (0, nil), which is a legal read that carries no bytes. Counting one
// as progress would keep the idle clock alive on a stream that is silent.
type emptyReader struct {
	ctx      context.Context
	interval time.Duration
}

func (r *emptyReader) Read([]byte) (int, error) {
	select {
	case <-time.After(r.interval):
		return 0, nil
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	}
}

// scriptedReader is a progress stream written down in advance: it hands out
// payload on its first read and then does whatever the stream is meant to do
// next.
//
//   - then set, atOnce false: the payload arrives, and every read after it fails
//     with then. The failure is STICKY because a real stream's is — a reader that
//     produced its error once and then something else would not be a stream at
//     all — and because the decoder above may well read again after the bytes it
//     already holds.
//   - then set, atOnce true: the payload and then arrive TOGETHER, in one read.
//     That is the shape the end of a short response takes, the last bytes and the
//     io.EOF in the same call.
//   - then unset: the payload arrives and the stream goes quiet, unblocking only
//     once the pull is aborted — a connection that went half-open after getting a
//     few bytes out.
type scriptedReader struct {
	ctx     context.Context
	payload string
	then    error
	atOnce  bool
	sent    bool
}

func (r *scriptedReader) Read(p []byte) (int, error) {
	if !r.sent {
		r.sent = true

		n := copy(p, r.payload)
		if r.atOnce {
			return n, r.then
		}

		return n, nil
	}

	if r.then != nil {
		return 0, r.then
	}

	<-r.ctx.Done()

	return 0, r.ctx.Err()
}

// oneShotErrorReader is the reader whose failure is NOT sticky: it delivers bytes
// and an error in the SAME read — which the decoder swallows, because it drops the
// error of any read that produced bytes — and then ends cleanly, as though nothing
// had gone wrong.
//
// Real bodies do not behave like this; net/http and gzip both keep answering with
// the same error once they have one. That is precisely why it is worth a stub: the
// drain must not depend on a reader repeating itself to notice that the stream
// failed, because a failure noticed nowhere is a pull reported as a success.
type oneShotErrorReader struct {
	payload string
	failure error
	sent    bool
}

func (r *oneShotErrorReader) Read(p []byte) (int, error) {
	if r.sent {
		return 0, io.EOF
	}

	r.sent = true

	return copy(p, r.payload), r.failure
}

// busyReader answers instantly and endlessly without ever saying anything: after
// its payload, every read returns (0, nil) as fast as it is asked, and it ignores
// cancellation entirely. It is both things the drain protects itself against in one
// reader — the empty read that costs a whole core if it is retried at once, and the
// reader that will never end the loop on its own however dead the pull is.
//
// It counts its reads so that a drain which spins fails as an assertion rather than
// as a core burning quietly for the length of the stall.
type busyReader struct {
	payload string
	sent    bool
	reads   int
}

func (r *busyReader) Read(p []byte) (int, error) {
	r.reads++

	if !r.sent {
		r.sent = true

		return copy(p, r.payload), nil
	}

	return 0, nil
}

// newTestPuller builds a Puller talking to a stub daemon.
//
// The Docker client is given the SERVER's own http.Client rather than
// http.DefaultClient. The default one is process-global, so a test that left a
// setting or a pooled connection on it would be changing the ground under every
// other test in the binary; and it is closed with the puller, so no idle
// connection outlives the server it points at.
func newTestPuller(t *testing.T, srv *httptest.Server, stall time.Duration) *Puller {
	t.Helper()

	cli, err := dockerclient.NewClientWithOpts(
		dockerclient.WithHost(srv.URL),
		dockerclient.WithHTTPClient(srv.Client()),
	)
	require.NoError(t, err)

	t.Cleanup(func() {
		require.NoError(t, cli.Close())
	})

	return &Puller{
		client:         cli,
		registryClient: NewRegistryClient(testhelpers.NewDatastore()),
		stallTimeout:   stall,
	}
}

// pullWithin runs a Pull on a caller context with NO deadline, which is the
// property every Pull test here depends on: the recreate HTTP handler passes
// context.TODO(), so the stall bound is the only thing that can end a wedged pull,
// and giving the caller a deadline would let that deadline do the work instead and
// prove nothing.
//
// Which is why the pull runs in a goroutine raced against a timer. A Pull that
// failed to abort would block forever, and blocking in the test goroutine would
// surface as go test's ten-minute panic — taking down every other test in the
// package rather than failing this one. There is no require inside the goroutine
// on purpose: testifylint's go-require rule forbids it, because t.FailNow from a
// non-test goroutine does not actually stop the test.
func pullWithin(t *testing.T, limit time.Duration, puller *Puller, img Image) error {
	t.Helper()

	done := make(chan error, 1)

	go func() {
		done <- puller.Pull(t.Context(), img)
	}()

	select {
	case err := <-done:
		return err
	case <-time.After(limit):
		t.Fatalf("the pull was never abandoned within %s", limit)

		return nil
	}
}

// TestDrainProgressLetsALongButTalkativeStreamFinish is the property the whole
// change exists for: total elapsed time is not the question, silence is. A stream
// that keeps producing must be allowed to run far past the stall timeout, because
// the stall timeout is not a budget for the download — it is the length of the
// quiet the pull is allowed to have.
//
// This test is what fails if anyone reintroduces a total-duration cut: the stream
// here runs for well over twice the stall timeout while never being quiet for
// more than a fraction of it, so any bound measured from the START of the drain
// rather than from the LAST byte kills it.
func TestDrainProgressLetsALongButTalkativeStreamFinish(t *testing.T) {
	t.Parallel()

	// A tenth of the gap between chunks, matching the margin the other timing tests
	// here keep. The chunks are what the test is about, so the room has to be in the
	// stall: under -race on a loaded runner a couple of hundred milliseconds can pass
	// between a read completing and the next one being scheduled, and a stall only a
	// few times the gap would call that healthy stream stalled.
	const stall = 500 * time.Millisecond

	pullCtx, cancel := context.WithCancel(t.Context())
	defer cancel()

	// Twenty-four chunks, 50ms apart: 1.2s of stream — well over twice the stall
	// timeout, so a bound measured from the start of the drain still kills it — and
	// never more than a tenth of the stall timeout of quiet between two of them.
	progress := &steadyReader{ctx: pullCtx, interval: 50 * time.Millisecond, left: 24}

	start := time.Now()
	err := drainWithGuard(t.Context(), progress, stall, cancel)
	elapsed := time.Since(start)

	require.NoError(t, err, "a stream that keeps producing must be allowed to finish, however long it takes in total")
	require.Greater(t, elapsed, stall,
		"the stream has to outlive the stall timeout for this test to mean anything")
}

// TestDrainProgressAbortsASilentStream is the other half of the same property: a
// pull that has stopped producing is abandoned after the stall timeout and not a
// download later. The bound that used to decide this was the caller's absolute
// one — an hour in a recreate — which is the whole reason a wedged pull was worth
// detecting separately.
func TestDrainProgressAbortsASilentStream(t *testing.T) {
	t.Parallel()

	const stall = 100 * time.Millisecond

	pullCtx, cancel := context.WithCancel(t.Context())
	defer cancel()

	start := time.Now()
	err := drainWithGuard(t.Context(), &silentReader{ctx: pullCtx}, stall, cancel)
	elapsed := time.Since(start)

	require.ErrorContains(t, err, "made no progress",
		"a stalled pull must say it stalled, not report some transport error it did not have")
	require.NotErrorIs(t, err, context.Canceled,
		"the abort is ours, so it must not be dressed up as the caller changing its mind")
	require.GreaterOrEqual(t, elapsed, stall, "the abort must not fire before the pull has actually been quiet")
	require.Less(t, elapsed, 2*time.Second,
		"the point of the stall timeout is that it fires long before any absolute bound the caller carries")
}

// TestDrainProgressDoesNotRearmOnEmptyReads pins the one read that looks like
// activity and is not. (0, nil) is a legal return that carries nothing, and
// restarting the idle clock on it would keep a stream alive that has not said a
// word — precisely the state the detector exists to catch.
func TestDrainProgressDoesNotRearmOnEmptyReads(t *testing.T) {
	t.Parallel()

	const stall = 150 * time.Millisecond

	// The caller gets a bound far past the stall, purely so a drain that DID rearm on
	// empty reads fails an assertion rather than hanging the package: a stream that
	// answers forever without saying anything would otherwise never end, and
	// t.Context() carries no deadline of its own.
	ctx, cancelCaller := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancelCaller()

	pullCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	progress := &emptyReader{ctx: pullCtx, interval: 20 * time.Millisecond}

	start := time.Now()
	err := drainWithGuard(ctx, progress, stall, cancel)
	elapsed := time.Since(start)

	require.ErrorContains(t, err, "made no progress",
		"a stream that only ever returns (0, nil) is a silent one, however often it answers")
	require.GreaterOrEqual(t, elapsed, stall)
	require.Less(t, elapsed, time.Second,
		"the empty reads must not have kept the idle clock alive")
}

// TestDrainProgressReportsCallerCancellationAsCancellation pins the distinction
// that is easiest to get wrong. Aborting the pull cancels its context, so by the
// time the read fails the pull's own context reports cancellation whether the
// decision was ours or the caller's — only the PARENT context still knows. Get it
// backwards and a recreate that ran out of its own time is reported as a network
// stall, sending whoever reads the log after the wrong thing entirely.
func TestDrainProgressReportsCallerCancellationAsCancellation(t *testing.T) {
	t.Parallel()

	// The stall timeout is set far out of reach, so anything the drain returns here
	// came from the caller and could not have come from the idle clock.
	const stall = 30 * time.Second

	t.Run("cancelled", func(t *testing.T) {
		t.Parallel()

		ctx, cancelCaller := context.WithCancel(t.Context())
		pullCtx, cancel := context.WithCancel(ctx)
		defer cancel()

		time.AfterFunc(50*time.Millisecond, cancelCaller)

		start := time.Now()
		err := drainWithGuard(ctx, &silentReader{ctx: pullCtx}, stall, cancel)

		require.ErrorIs(t, err, context.Canceled)
		require.NotContains(t, err.Error(), "made no progress",
			"the caller gave up, the pull did not stall")
		require.Less(t, time.Since(start), stall)
	})

	t.Run("deadline", func(t *testing.T) {
		t.Parallel()

		ctx, cancelCaller := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancelCaller()

		pullCtx, cancel := context.WithCancel(ctx)
		defer cancel()

		start := time.Now()
		err := drainWithGuard(ctx, &silentReader{ctx: pullCtx}, stall, cancel)

		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.NotContains(t, err.Error(), "made no progress",
			"the caller ran out of time, the pull did not stall")
		require.Less(t, time.Since(start), stall)
	})
}

// TestDrainProgressReportsATransportFailureAsItself covers the branch the "was
// this abort ours?" flag exists for. A stream that dies of its own accord — a
// reset connection, a daemon that exits mid-pull — reaches the same place a stall
// does, and reporting it as a stall would point an operator at a network that is
// fine and away from the process that died.
func TestDrainProgressReportsATransportFailureAsItself(t *testing.T) {
	t.Parallel()

	// Far out of reach, so a stall verdict here could only be a misclassification.
	const stall = 30 * time.Second

	reset := errors.New("read: connection reset by peer")

	pullCtx, cancel := context.WithCancel(t.Context())
	defer cancel()

	progress := &scriptedReader{ctx: pullCtx, payload: progressLine, then: reset}

	start := time.Now()
	err := drainWithGuard(t.Context(), progress, stall, cancel)

	require.ErrorIs(t, err, reset, "the failure the stream reported must survive to the caller intact")
	require.NotContains(t, err.Error(), "made no progress",
		"the connection died; it did not go quiet, and the two send an operator to different places")
	require.NotContains(t, err.Error(), "ran out of time")
	require.Less(t, time.Since(start), stall)
}

// TestDrainProgressFailsAStreamCutShortOfItsEnd covers the end that LOOKS clean
// and is not: a download whose connection is dropped part-way through, which io
// reports as an unexpected EOF and which must never be read as a pull that
// finished.
//
// Two things can carry the evidence of the cut, and the cases below are sorted by
// which one does. The reader may say so itself, by answering io.ErrUnexpectedEOF —
// what a body cut off mid-chunk keeps returning — and then the drain has only to
// take its word for it. Or the reader may end perfectly cleanly, with a plain
// io.EOF, and then the ONLY trace left is the unterminated message sitting in the
// decoder's buffer. Both are ordinary: the boundary cut is what a dropped
// connection usually looks like, because the daemon flushes each progress line as
// its own chunk, and a clean end after half a line is what an intermediary that
// closed the connection deliberately looks like.
//
// A pull reported as successful in any of them would have Recreate rebuild the
// container from the stale local image, which is the failure the whole drain exists
// to stop. And an intermediary makes these likely rather than exotic: the longer a
// download runs, the more chance a proxy, an agent tunnel or a restarted daemon has
// to close the connection under it.
func TestDrainProgressFailsAStreamCutShortOfItsEnd(t *testing.T) {
	t.Parallel()

	// Far out of reach, so a stall verdict here could only be a misclassification.
	// The stream does not go quiet — it ends, and says so.
	const stall = 30 * time.Second

	tests := []struct {
		name    string
		payload string
		// then is how the reader ends, and it is the whole point of the table: the same
		// truncated bytes have to be caught whether the reader admits to the cut or ends
		// as politely as a stream that finished.
		then error
	}{
		{
			name: "cut on a message boundary",
			// Whole messages and then nothing, which is the ordinary shape of a dropped
			// connection rather than a corner of it. Nothing is left half-parsed, so the
			// decoder reports the same io.EOF a finished pull gives — the truncation
			// survives only in the error the READER last returned, which is why the drain
			// has to consult it before calling a stream finished.
			payload: progressLine,
			then:    io.ErrUnexpectedEOF,
		},
		{
			name: "cut in the middle of a message",
			// The truncation is in the bytes here, but that is not what produces the
			// verdict: the decoder's complaint about them is an io.ErrUnexpectedEOF, and
			// so is the error this reader keeps returning, so the drain recognises it as
			// the failure the STREAM reported and classifies it as one. The two are the
			// same sentinel, which is exactly why this case cannot tell the two mechanisms
			// apart — the case below is the one that can.
			payload: partialLine,
			then:    io.ErrUnexpectedEOF,
		},
		{
			name: "cut in the middle of a message, on a reader that ends cleanly",
			// The same truncated bytes, ended the way an intermediary ends a connection it
			// chose to close: politely. The reader's last word is a plain io.EOF, so
			// nothing outside the decoder knows anything was lost, and asking the reader
			// gets the answer a finished pull would give. What is left is the unterminated
			// message in the decoder's buffer, and the drain has to treat the decoder's own
			// io.ErrUnexpectedEOF as the proof of truncation it is — a stream still valid
			// when it ran out is a stream that was cut.
			payload: partialLine,
			then:    io.EOF,
		},
		{
			name: "cut once the parsing had already given up",
			// The fallback drain sees the reader's error unchanged rather than through
			// the decoder, and has to reach the same verdict; this is what says so
			// instead of assuming it.
			payload: garbageLine + progressLine,
			then:    io.ErrUnexpectedEOF,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			pullCtx, cancel := context.WithCancel(t.Context())
			defer cancel()

			progress := &scriptedReader{ctx: pullCtx, payload: test.payload, then: test.then}

			start := time.Now()
			err := drainWithGuard(t.Context(), progress, stall, cancel)

			require.ErrorIs(t, err, io.ErrUnexpectedEOF,
				"a download cut short is a failed pull, and it has to be reported as the transport failure it was")
			require.NotContains(t, err.Error(), "made no progress",
				"the stream did not go quiet; it ended early, and the two send an operator to different places")
			require.NotContains(t, err.Error(), "ran out of time")
			require.Less(t, time.Since(start), stall)
		})
	}
}

// TestDrainProgressKeepsAFailureAReaderReportedOnlyOnce covers the failure that is
// said once and never again. A reader may return bytes and an error in the same
// call, and the decoder drops the error of any read that produced bytes — so that
// failure is recorded in one place only, and if a later clean io.EOF is allowed to
// overwrite it, the drain asks how the stream ended, hears "cleanly", and reports a
// pull that broken as an updated image.
//
// Production readers are sticky and would report it again, which is what makes this
// worth pinning rather than leaving to luck: the property being kept is that the
// drain does not DEPEND on them doing so.
func TestDrainProgressKeepsAFailureAReaderReportedOnlyOnce(t *testing.T) {
	t.Parallel()

	// Far out of reach, so a stall verdict here could only be a misclassification.
	const stall = 30 * time.Second

	reset := errors.New("read: connection reset by peer")

	_, cancel := context.WithCancel(t.Context())
	defer cancel()

	progress := &oneShotErrorReader{payload: progressLine, failure: reset}

	err := drainWithGuard(t.Context(), progress, stall, cancel)

	require.ErrorIs(t, err, reset,
		"the stream reported a failure once, and an end of stream afterwards does not undo it")
	require.NotContains(t, err.Error(), "made no progress")
	require.NotContains(t, err.Error(), "ran out of time")
}

// TestDrainProgressReadsBytesDeliveredWithEOF covers the read a reader is allowed
// to make and a short response often does: the last bytes and the end of the
// stream in ONE call. A drain that looked at the error before the bytes would
// throw those bytes away — and with them any failure reported in the last message,
// which is exactly where the daemon puts it.
func TestDrainProgressReadsBytesDeliveredWithEOF(t *testing.T) {
	t.Parallel()

	t.Run("a finished pull", func(t *testing.T) {
		t.Parallel()

		pullCtx, cancel := context.WithCancel(t.Context())
		defer cancel()

		progress := &scriptedReader{
			ctx:     pullCtx,
			payload: `{"status":"Status: Downloaded newer image for nginx:latest"}` + "\n",
			then:    io.EOF,
			atOnce:  true,
		}

		require.NoError(t, drainWithGuard(t.Context(), progress, time.Minute, cancel))
	})

	t.Run("a failure reported in the last message", func(t *testing.T) {
		t.Parallel()

		pullCtx, cancel := context.WithCancel(t.Context())
		defer cancel()

		progress := &scriptedReader{ctx: pullCtx, payload: failureLine, then: io.EOF, atOnce: true}

		require.ErrorContains(t, drainWithGuard(t.Context(), progress, time.Minute, cancel),
			"no space left on device",
			"the failure arrived in the same read as the end of the stream and must not be lost with it")
	})
}

// TestDrainProgressAcceptsAnOrdinaryProgressStream is the guard on the whole
// parsing change: the common case is a pull that WORKS, and reading the messages
// must not cost it. Every shape a real pull emits is here, the aux record at the
// end included.
func TestDrainProgressAcceptsAnOrdinaryProgressStream(t *testing.T) {
	t.Parallel()

	stream := strings.Join([]string{
		`{"status":"Pulling from library/nginx","id":"latest"}`,
		`{"status":"Pulling fs layer","progressDetail":{},"id":"a1b2c3d4"}`,
		`{"status":"Downloading","progressDetail":{"current":1024,"total":8192},"progress":"[===>  ] 1kB/8kB","id":"a1b2c3d4"}`,
		`{"status":"Verifying Checksum","progressDetail":{},"id":"a1b2c3d4"}`,
		`{"status":"Extracting","progressDetail":{"current":8192,"total":8192},"id":"a1b2c3d4"}`,
		`{"status":"Digest: sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}`,
		`{"status":"Status: Downloaded newer image for nginx:latest"}`,
		`{"aux":{"ID":"sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"}}`,
	}, "\n") + "\n"

	// The abort has a context of its own, as in production, but nothing here reads
	// it: a strings.Reader cannot be interrupted and does not need to be.
	_, cancel := context.WithCancel(t.Context())
	defer cancel()

	require.NoError(t, drainWithGuard(t.Context(), strings.NewReader(stream), time.Minute, cancel),
		"a pull the daemon completed must stay a success once its commentary is read rather than discarded")
}

// TestDrainProgressFailsOnAnInBandError is the failure this package used to
// report as a SUCCESS. POST /images/create sends its 200 before most of the pull
// happens, so everything that goes wrong afterwards is said in the body and then
// the stream simply ends — which io.ReadAll and a byte drain both read as a pull
// that finished.
//
// Reporting it as success is worse than losing the message: Recreate then builds
// the new container from the image NAME, which on an auto-update still resolves to
// the digest already on disk, so the workload is stopped, recreated from the STALE
// image and restarted, the recreate claims success, and the next status check finds
// it outdated and does it again — a restart loop with no error anywhere in it.
func TestDrainProgressFailsOnAnInBandError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		stream string
		want   string
	}{
		{
			name:   "at the end of the stream",
			stream: progressLine + failureLine,
			want:   "no space left on device",
		},
		{
			name: "with lines after it",
			// The daemon normally stops writing here, but a stream that carries on must
			// not be able to bury the failure under what follows it. That is all this
			// pins: the failure is not lost, not that it is acted on the instant it
			// arrives. Decode fills a 32 KiB buffer before it returns anything, so
			// whether these lines are read before or after the failure is noticed is up
			// to how the bytes happened to arrive — which costs nothing, because the
			// daemon closes the stream right after writing an errorDetail message.
			stream: progressLine + failureLine + progressLine,
			want:   "no space left on device",
		},
		{
			name: "after a message the decoder could not fit",
			// A message that parses but carries a field of a type this struct does not
			// declare is consumed whole before the mismatch is reported, so the decoder
			// stays framed and the stream is still worth reading. Giving up on the
			// checking there would hide every failure reported after it.
			stream: `{"status":42}` + "\n" + progressLine + failureLine,
			want:   "no space left on device",
		},
		{
			name: "only the deprecated error field",
			// errorDetail is what the daemon fills; "error" is its older twin, and the
			// only one left if something between here and the daemon rewrites the stream.
			stream: `{"error":"toomanyrequests: You have reached your pull rate limit"}` + "\n",
			want:   "toomanyrequests: You have reached your pull rate limit",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, cancel := context.WithCancel(t.Context())
			defer cancel()

			err := drainWithGuard(t.Context(), strings.NewReader(test.stream), time.Minute, cancel)

			require.ErrorContains(t, err, test.want, "the daemon's own account of the failure has to reach the caller")
			require.ErrorContains(t, err, "the daemon reported the image pull as failed",
				"the outcome has to be told apart from a stall and from a dead connection")
			require.NotContains(t, err.Error(), "made no progress")
			require.NotErrorIs(t, err, context.Canceled)
			require.NotErrorIs(t, err, context.DeadlineExceeded)
		})
	}
}

// TestDrainProgressDegradesOnAMalformedStream is the safety property of parsing
// the stream at all, and the risk that was knowingly taken with it. Reading the
// messages may only ever turn a failure that was SILENT into a loud one; it must
// never invent a failure that did not exist. So a stream the decoder cannot make
// sense of — a malformed line, a shape nobody expected, framing this code does not
// know — falls back to draining the bytes and ends exactly as it did before:
// success at the end of the stream.
func TestDrainProgressDegradesOnAMalformedStream(t *testing.T) {
	t.Parallel()

	t.Run("a stream it cannot read is still a pull that succeeded", func(t *testing.T) {
		t.Parallel()

		_, cancel := context.WithCancel(t.Context())
		defer cancel()

		require.NoError(t, drainWithGuard(t.Context(), strings.NewReader(garbageLine), time.Minute, cancel),
			"a pull the daemon completed must not start failing because Portainer could not read a line of its commentary")
	})

	t.Run("the idle clock survives giving up on parsing", func(t *testing.T) {
		t.Parallel()

		const stall = 150 * time.Millisecond

		pullCtx, cancel := context.WithCancel(t.Context())
		defer cancel()

		// Bytes it cannot parse, and then a connection that goes half-open. The
		// fallback drain reads through the same reader that resets the idle clock, so
		// degrading must not cost the stall detection along with the parsing.
		progress := &scriptedReader{ctx: pullCtx, payload: garbageLine}

		start := time.Now()
		err := drainWithGuard(t.Context(), progress, stall, cancel)
		elapsed := time.Since(start)

		require.ErrorContains(t, err, "made no progress")
		require.GreaterOrEqual(t, elapsed, stall)
		require.Less(t, elapsed, 2*time.Second)
	})

	t.Run("a reader that answers instantly and endlessly is paced rather than spun on", func(t *testing.T) {
		t.Parallel()

		const stall = 100 * time.Millisecond

		// The payload has to FILL the decoder's buffer, not merely be unparseable: the
		// decoder reads with io.ReadFull, which returns only once the free space is full
		// or the reader fails, and this reader does neither on its own. 32 KiB of nonsense
		// is what gets the decoder to give up and hand the rest to the byte drain, which
		// is where the reader then answers (0, nil) as fast as it is asked, forever, and
		// never looks at a cancelled context. Nothing bounds how often it is called except
		// the pause after an empty read, and nothing ends the loop except the drain
		// checking the idle clock itself.
		progress := &busyReader{payload: strings.Repeat(garbageLine, 1024)}

		_, cancel := context.WithCancel(t.Context())
		defer cancel()

		// Raced against a timer, and for the reason pullWithin is: nothing this reader
		// does can end the drain, so a drain that stopped checking the idle clock itself
		// would loop here forever and surface as go test's ten-minute panic, taking the
		// whole package down instead of failing this one test. Reading the counter after
		// the drain has reported back is also what keeps it off two goroutines at once.
		done := make(chan error, 1)

		start := time.Now()

		go func() {
			done <- drainWithGuard(t.Context(), progress, stall, cancel)
		}()

		var err error

		select {
		case err = <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("the drain never ended, and this reader was never going to end it")
		}

		elapsed := time.Since(start)

		require.ErrorContains(t, err, "made no progress",
			"this reader will never end the drain, so the drain has to end itself on the idle clock")
		require.GreaterOrEqual(t, elapsed, stall)
		require.Less(t, elapsed, 2*time.Second)

		// A millisecond of pause per empty read puts the ceiling at about one read per
		// millisecond of stall — a hundred of them here. Ten times that is generous room
		// for a loaded runner while staying orders of magnitude under what an unpaced
		// loop reaches, which is millions of reads in the same hundred milliseconds.
		require.Less(t, progress.reads, 10*int(stall/time.Millisecond),
			"an empty read has to cost a brief wait rather than an immediate retry")
	})
}

// TestStallBudgetFallsBackToTheDefault covers the zero value. A Puller built by
// hand rather than through NewPuller carries no stall timeout, and a zero idle
// deadline is one that has already expired: read straight, it would abort every
// pull before its first chunk could arrive, which is not a stricter stall
// detector but a pull that can never succeed.
func TestStallBudgetFallsBackToTheDefault(t *testing.T) {
	t.Parallel()

	bare := &Puller{}
	require.Zero(t, bare.stallTimeout, "the unset timeout is the point of the test")
	require.Equal(t, defaultPullStallTimeout, bare.stallBudget(),
		"a zero stall timeout must not abort every pull on the spot")

	require.Equal(t, defaultPullStallTimeout, NewPuller(nil, nil, nil).stallBudget(),
		"the constructor arms the default")

	require.Equal(t, 5*time.Millisecond, (&Puller{stallTimeout: 5 * time.Millisecond}).stallBudget(),
		"a configured timeout is used as given")

	require.GreaterOrEqual(t, defaultPullStallTimeout, 30*time.Second,
		"the default has to clear the quiet gaps a healthy pull has — a cold registry starting a layer, the final config commit")
}

// TestPullAbortsAStalledHTTPBodyRead proves the mechanism against a REAL blocked
// body read rather than a hand-written reader. This is the part that could look
// correct and do nothing: a read parked inside net/http unblocks when the
// REQUEST's context is cancelled and in no other way, so a Pull that issued
// ImagePull on the caller's context and cancelled some other one would hang here
// until the caller's own deadline — which on the recreate path is an hour away.
func TestPullAbortsAStalledHTTPBodyRead(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)

		// One real chunk, flushed, so the client has a live stream with progress on
		// it — then silence, holding the response open the way a half-open
		// connection does, until the client tears the request down.
		_, _ = io.WriteString(w, `{"status":"Pulling from library/nginx"}`+"\n")

		if ok {
			flusher.Flush()
		}

		<-r.Context().Done()
	}))
	defer srv.Close()

	puller := newTestPuller(t, srv, 200*time.Millisecond)

	img, err := ParseImage(ParseImageOptions{Name: "nginx:latest"})
	require.NoError(t, err)

	start := time.Now()
	err = pullWithin(t, 5*time.Second, puller, img)
	elapsed := time.Since(start)

	require.ErrorContains(t, err, "made no progress")
	require.Less(t, elapsed, 5*time.Second, "the blocked body read must actually be cut, not merely be given up on")
}

// TestPullAbortsAPullStuckBeforeTheResponseHeaders covers the phase an idle clock
// armed inside the drain cannot see at all, which is also the longest guaranteed
// silent one. ImagePull returns when the response HEADERS arrive, and the daemon
// writes no header until its first write to the body, while its first line comes
// only once the manifest has been fetched — so registry authentication and
// manifest resolution happen entirely inside that call. A pull wedged there with
// the clock started afterwards hangs for the caller's whole budget: an hour on the
// manual recreate path, which has no deadline of its own at all.
func TestPullAbortsAPullStuckBeforeTheResponseHeaders(t *testing.T) {
	t.Parallel()

	const stall = 200 * time.Millisecond

	released := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Nothing written, nothing flushed: the connection is accepted and then the
		// response headers never leave the server, exactly as they do not while the
		// daemon is still talking to the registry.
		select {
		case <-r.Context().Done():
		case <-released:
		}
	}))

	// Ordered so the handler is let go BEFORE Close waits on it. Close blocks until
	// its outstanding requests are done, so a handler still parked when this test
	// gave up would hang the whole package instead of failing this one test.
	defer srv.Close()
	defer close(released)

	puller := newTestPuller(t, srv, stall)

	img, err := ParseImage(ParseImageOptions{Name: "nginx:latest"})
	require.NoError(t, err)

	start := time.Now()
	err = pullWithin(t, 5*time.Second, puller, img)
	elapsed := time.Since(start)

	require.ErrorContains(t, err, "made no progress",
		"a pull wedged on the registry handshake is a stall like any other, and the clock has to be running through it")
	require.GreaterOrEqual(t, elapsed, stall, "the abort must not fire before the pull has actually been quiet")
	require.Less(t, elapsed, 5*time.Second,
		"the stall bound is the only thing that can end this pull; the caller carries no deadline")
}

// TestPullReportsAFailedRequestAsItself is the guard on arming the idle clock
// before the request: a request that fails on its own and fails FAST must still be
// reported as what it was. The clock is running by then, so a classification that
// looked only at whether a timer exists would relabel every early failure — a
// refused connection, a daemon answering 500 — as a stall.
func TestPullReportsAFailedRequestAsItself(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"message":"Get \"https://registry-1.docker.io/v2/\": dial tcp: no such host"}`)
	}))
	defer srv.Close()

	// The stall bound is far out of reach, so a stall verdict here could only come
	// from the clock armed before the request being read as the cause of a failure it
	// had nothing to do with.
	puller := newTestPuller(t, srv, 30*time.Second)

	img, err := ParseImage(ParseImageOptions{Name: "nginx:latest"})
	require.NoError(t, err)

	start := time.Now()
	err = pullWithin(t, 5*time.Second, puller, img)

	require.ErrorContains(t, err, "no such host", "the daemon's own answer must reach the caller")
	require.NotContains(t, err.Error(), "made no progress")
	require.NotContains(t, err.Error(), "ran out of time")
	require.Less(t, time.Since(start), 5*time.Second, "the failure was immediate and must be reported immediately")
}
