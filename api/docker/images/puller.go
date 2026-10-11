package images

import (
	"context"
	"io"
	"sync/atomic"
	"time"

	"github.com/portainer/portainer/api/dataservices"
	"github.com/portainer/portainer/api/logs"

	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/jsonmessage"
	"github.com/pkg/errors"
	"github.com/rs/zerolog/log"
	"github.com/segmentio/encoding/json"
)

// defaultPullStallTimeout is how long the pull may go without a single byte of
// progress before it is abandoned. It fires on a stuck pull, never on a big one.
//
// The classic image store emits progress per 512 KiB moved per layer (the 100ms
// limiter in pkg/progress/progressreader.go is a ceiling, not a heartbeat), so a
// minute of silence means a layer moved under ~8.5 KiB/s. The minute also clears
// the silent waits on the registry: a pull-through cache fetching a cold blob,
// and token auth plus manifest resolution before "Pulling from <repo>", which
// happen inside ImagePull (see Pull).
//
// The caller keeps its own absolute bound (defaultPullTimeout in api/docker) for
// a pull that dribbles bytes forever.
const defaultPullStallTimeout = 60 * time.Second

// progressBufferSize is the read size of the fallback drain, the one that runs
// once the stream has stopped making sense as JSON. The bytes are read only to be
// counted and thrown away — exactly as io.ReadAll's result was — so this is a
// scratch buffer, not a bound on anything, and it is a fixed one so a long pull
// cannot accumulate its whole progress stream in memory the way io.ReadAll did.
const progressBufferSize = 32 * 1024

// emptyReadBackoff is how long the drain waits after a read that returned no
// bytes and no error, so that a reader answering instantly and endlessly costs a
// wakeup per millisecond instead of a whole core. It is short enough to be
// invisible to any real stream, which never reads this way at all.
const emptyReadBackoff = time.Millisecond

type Puller struct {
	client         *client.Client
	registryClient *RegistryClient
	dataStore      dataservices.DataStore
	// stallTimeout is how long the pull may produce nothing before it is
	// abandoned. It is a field rather than a constant read at the call site so a
	// test can drive a stalled pull in milliseconds without mutating a
	// package-level knob shared by every other (parallel) test. Read it through
	// stallBudget rather than directly, so the zero value cannot turn every pull
	// into one that is aborted before it has begun.
	stallTimeout time.Duration
}

func NewPuller(client *client.Client, registryClient *RegistryClient, dataStore dataservices.DataStore) *Puller {
	return &Puller{
		client:         client,
		registryClient: registryClient,
		dataStore:      dataStore,
		stallTimeout:   defaultPullStallTimeout,
	}
}

// stallBudget is how long the pull may go without progress, and the only reading
// of stallTimeout there is.
//
// A zero stallTimeout (a Puller built without NewPuller) falls back to the
// default, since a zero idle deadline would abort every pull at once.
func (puller *Puller) stallBudget() time.Duration {
	if puller.stallTimeout <= 0 {
		return defaultPullStallTimeout
	}

	return puller.stallTimeout
}

// stallGuard is the idle clock one pull runs under. It aborts the pull as soon as
// the pull has produced nothing for stall, and it remembers that the abort was ITS
// doing, so the outcome can be told apart from everything else that ends a pull
// the same way.
//
// The clock spans two phases in different functions: the request Pull issues and
// the drain that follows it.
type stallGuard struct {
	stall time.Duration
	timer *time.Timer
	// fired records that the abort was OUR decision. Without it a genuine
	// transport failure — a reset connection, a daemon that exits mid-pull — would
	// reach the same error branch and be reported as a stall, which would send an
	// operator looking at the network when the daemon is the thing that died.
	fired atomic.Bool
}

// newStallGuard arms the clock. It is armed, never merely created: from this
// moment the pull has stall to produce its first byte, and abort runs if it does
// not. The caller owns stop.
func newStallGuard(stall time.Duration, abort context.CancelFunc) *stallGuard {
	guard := &stallGuard{stall: stall}

	guard.timer = time.AfterFunc(stall, func() {
		guard.fired.Store(true)
		abort()
	})

	return guard
}

// progressed restarts the idle clock, and is called for every read that produced
// bytes.
//
// It skips the rearm once the abort has fired, since Reset on an AfterFunc timer
// that has already run schedules a fresh run. The check is advisory: abort is
// idempotent and stop takes any rearmed timer down.
func (guard *stallGuard) progressed() {
	if !guard.fired.Load() {
		guard.timer.Reset(guard.stall)
	}
}

func (guard *stallGuard) stop() {
	guard.timer.Stop()
}

// outcome names what actually ended the pull, given the error that surfaced and
// the CALLER's context — not the pull's. Passing the caller's is what lets the
// outcomes be told apart, and there is no other way to tell them apart: once the
// abort has run, the pull's own context reports cancellation whether the decision
// was ours or the caller's, so only the parent still knows whose it was.
func (guard *stallGuard) outcome(ctx context.Context, err error) error {
	// The caller's own deadline or cancellation, checked before the stall so the log
	// tells "ran out of time" from "went silent". The wording does not name whose
	// bound fired, since pullBudget may have shortened the caller's; errors.Is
	// still tells cancellation from expiry.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return errors.Wrap(ctxErr, "image pull ran out of time")
	}

	if guard.fired.Load() {
		return errors.Errorf("image pull made no progress for %s", guard.stall)
	}

	// Anything else is a real failure of the exchange itself and is reported as
	// what it is.
	return err
}

func (puller *Puller) Pull(ctx context.Context, img Image) error {
	log.Debug().Str("image", img.FullName()).Msg("starting to pull the image")

	registryAuth, err := puller.registryClient.EncodedRegistryAuth(img)
	if err != nil {
		log.Debug().
			Str("image", img.FullName()).
			Err(err).
			Msg("failed to get an encoded registry auth via image, try to pull image without registry auth")
	}

	// The request is issued on a cancellable context of its own, derived from the
	// caller's: a blocked body read unblocks only when the request's context is
	// cancelled, which is how the stall abort works.
	pullCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// The idle clock is armed before the request: ImagePull returns only when the
	// headers arrive, and the daemon sends none until its first body write
	// ("Pulling from <repo>", after the manifest is fetched), so registry auth and
	// manifest resolution happen silently inside this call. The drain below takes
	// over the same clock.
	guard := newStallGuard(puller.stallBudget(), cancel)
	defer guard.stop()

	out, err := puller.client.ImagePull(pullCtx, img.FullName(), image.PullOptions{
		RegistryAuth: registryAuth,
	})
	if err != nil {
		// Either the request failed on its own — a refused connection, a daemon that
		// answered 500 — or it failed because the guard above just cancelled it. Only
		// outcome can tell those apart, and it leaves a fast, genuine failure exactly
		// as it was.
		return guard.outcome(ctx, err)
	}
	defer logs.CloseAndLogErr(out)

	return drainProgress(ctx, out, guard)
}

// progressStream is the raw byte layer of the drain. It hands the bytes on
// unchanged and restarts the idle clock on every read that produced any.
//
// The JSON decoder sits on top of this so that stall detection counts bytes, not
// messages: a connection that dies halfway through a line is stalled too.
//
// err is the reader's own account of how the stream ended, which tells a failed
// stream from bytes the decoder could not parse. Only the draining goroutine
// touches it.
type progressStream struct {
	inner io.Reader
	guard *stallGuard
	err   error
}

func (stream *progressStream) Read(p []byte) (int, error) {
	n, err := stream.inner.Read(p)

	// Any byte is progress, whatever it turns out to say.
	if n > 0 {
		stream.guard.progressed()
	}

	// The reader's last word on how the stream ended, except that a clean io.EOF
	// never erases an earlier failure: the decoder drops the error of a read that
	// also produced bytes, so such a failure reaches the drain only through here.
	if err != nil && (stream.err == nil || !errors.Is(err, io.EOF)) {
		stream.err = err
	}

	// A read of no bytes and no error is legal; pause briefly instead of spinning a
	// core until the stall timeout. The pause lives here because the decoder reads
	// through io.ReadFull, which never hands control back to a loop that could pause.
	if n == 0 && err == nil {
		time.Sleep(emptyReadBackoff)
	}

	return n, err
}

// drainProgress reads the pull's progress stream to its end, aborting the pull as
// soon as it has produced nothing for the guard's stall timeout, and failing it
// if the daemon reports a failure in band.
//
// ctx is the CALLER's context, not the pull's; see stallGuard.outcome for why
// that distinction is the only way the outcomes can be told apart.
func drainProgress(ctx context.Context, progress io.Reader, guard *stallGuard) error {
	stream := &progressStream{inner: progress, guard: guard}

	// The decoder keeps one compacting buffer, so memory stays bounded at roughly
	// the larger of 32 KiB and twice the longest message.
	//
	// Decode does not return on the first newline: it fills its buffer with
	// io.ReadFull, so a failure line is acted on once the buffer fills or the reader
	// ends. moby closes the stream right after an errorDetail message, so that is
	// prompt.
	decoder := json.NewDecoder(stream)

	// Whether a message that parsed but did not fit the struct has already been
	// logged. A mismatch that comes from the shape of the stream rather than from one
	// odd line would otherwise repeat for every message a long pull writes, which is
	// thousands of them, and the first one says everything the rest would.
	reportedTypeMismatch := false

	for {
		var msg jsonmessage.JSONMessage

		err := decoder.Decode(&msg)
		if err == nil {
			if failure := inBandError(msg); failure != nil {
				return failure
			}

			continue
		}

		// The decoder's io.EOF is not proof of a clean end: readValue rewrites a
		// zero-byte io.ErrUnexpectedEOF (net/http's chunked reader on a truncated body)
		// to io.EOF, and the daemon flushes each line as its own chunk, so a dropped
		// connection usually breaks on a message boundary. Success needs the reader to
		// agree: no error, or an io.EOF of its own. Checking the reader rather than the
		// caller's context lets a cancellation racing the last byte still succeed.
		if errors.Is(err, io.EOF) {
			if stream.err != nil && !errors.Is(stream.err, io.EOF) {
				return guard.outcome(ctx, stream.err)
			}

			return nil
		}

		// An error the STREAM itself produced is a real failure of the exchange and is
		// classified as one. Asking the stream rather than pattern-matching the
		// decoder's error types is what makes the split exact: the decoder returns a
		// read error verbatim, so an error that is not the one the reader last handed
		// out did not come from the wire.
		if stream.err != nil && errors.Is(err, stream.err) {
			return guard.outcome(ctx, err)
		}

		// The decoder's own verdict that the stream stopped mid-value: an unterminated
		// but valid JSON prefix, even when the reader ended cleanly (a connection
		// closed half a line in). That is truncation by definition; non-JSON bytes fail
		// as *json.SyntaxError and take the forgiving path below instead.
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return guard.outcome(ctx, err)
		}

		// A message that parsed but did not fit the struct is skipped: the value was
		// consumed whole, so the decoder is still on a message boundary. A syntax error
		// leaves no boundary to resume from, so it goes to the fallback below.
		var typeMismatch *json.UnmarshalTypeError
		if errors.As(err, &typeMismatch) {
			if !reportedTypeMismatch {
				reportedTypeMismatch = true

				log.Warn().
					Err(err).
					Msg("cannot read a message of the image pull progress stream, skipping it and carrying on")
			}

			continue
		}

		// Anything else is unparseable input: drain the rest and treat EOF as success.
		// Parsing may turn a silent failure into a loud one but must never invent one,
		// so a parse problem is only a warning.
		log.Warn().
			Err(err).
			Msg("cannot read the image pull progress stream, falling back to draining it without checking for reported failures")

		return drainBytes(ctx, stream, guard)
	}
}

// inBandError is the failure the daemon reports INSIDE a successful response.
//
// POST /images/create has already sent its 200 by the time most of a pull
// happens, so everything that goes wrong after the first flushed byte can only be
// said in the body: a {"errorDetail":...} message followed by a clean end of
// stream (exhausted retries, digest mismatch, no space left, registry 5xx).
//
// Ignoring it would let Recreate rebuild the container from the stale local image
// under the same tag and report success, and auto-update would loop on it.
func inBandError(msg jsonmessage.JSONMessage) error {
	if msg.Error == nil && msg.ErrorMessage == "" {
		return nil
	}

	// errorDetail is the field the daemon fills; "error" is its deprecated twin,
	// still written alongside it and the only one left if something between here and
	// the daemon rewrites the stream. Either one present means the pull failed, and
	// the message is quoted rather than summarised because it is the daemon's own
	// account of what went wrong.
	detail := msg.ErrorMessage
	if msg.Error != nil && msg.Error.Message != "" {
		detail = msg.Error.Message
	}

	if detail == "" {
		detail = "no detail given"
	}

	return errors.Errorf("the daemon reported the image pull as failed: %s", detail)
}

// drainBytes reads what is left of the stream and throws it away. The reader it
// is given resets the idle clock, so a stream that then goes silent is still
// abandoned as a stall.
//
// Unlike the decoding loop, it may take io.EOF at face value: no decoder sits in
// between to rewrite io.ErrUnexpectedEOF.
func drainBytes(ctx context.Context, progress io.Reader, guard *stallGuard) error {
	buf := make([]byte, progressBufferSize)

	for {
		// Check the idle clock and the caller before every read: both only cancel a
		// context, which a reader that never consults one ignores.
		if ctx.Err() != nil || guard.fired.Load() {
			return guard.outcome(ctx, nil)
		}

		// A read of no bytes and no error is legal and carries nothing, so there is
		// nothing to do with it but read again. The pause that keeps that from becoming a
		// hot loop lives in progressStream.Read, which is the reader this drain is given
		// and the one the decoder above reads through as well; waiting there rather than
		// failing here is what keeps the degraded drain from inventing a failure the pull
		// never had, since a reader that says nothing for long enough is abandoned by the
		// check above as the stall it is.
		if _, err := progress.Read(buf); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}

			return guard.outcome(ctx, err)
		}
	}
}
