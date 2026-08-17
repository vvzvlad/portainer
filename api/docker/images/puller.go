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
// progress before it is abandoned.
//
// It is the bound that does not have to be guessed. Every other bound on a pull
// is a duration, and a duration is an answer to "how big are our images and how
// fast is the link today" — an hour is generous for a 200 MB image on home fibre
// and nowhere near enough for a 10 GB one over a 1 MB/s uplink, and neither
// install gets to pick. Absence of progress asks nothing about size or speed: a
// pull that is working streams JSON progress lines continuously, whatever it is
// pulling and however slowly, and a pull whose connection has gone half-open
// stops producing them at once. So this bound fires on the pull being STUCK,
// which is the thing worth failing on, and never on the pull merely being big.
//
// A minute has to sit above the WORST cadence a healthy pull keeps, not the
// best, and the two image stores do not keep the same one:
//
//   - The containerd store does write on a clock. A 100ms ticker keeps emitting
//     Extracting with an elapsed-seconds counter for as long as a layer is
//     unpacking, unconditionally and explicitly so the unpack does not look
//     silent (daemon/containerd/progress.go).
//   - The classic store does not. Its progress is BYTE-driven: the progress
//     reader calls updateProgress only once it has passed 512 KiB since the last
//     update — or 1% of the layer, when 1% is smaller — and only then consults a
//     limiter of one update per 100ms, which may drop the update anyway
//     (pkg/progress/progressreader.go). The 100ms there is a CEILING on how often
//     a line may be written, never a heartbeat: the gap between two Downloading
//     lines is however long that layer takes to move its next 512 KiB.
//
// The classic store is therefore what sets the floor, and reasoning from the
// 100ms figure alone would set it far too low: "the cadence is 100ms, so ten
// seconds has room to spare" comes out at about 50 KiB/s per layer and would
// abort real pulls. Read off the byte-driven cadence instead, a minute of silence
// on a layer means that layer moved less than 512 KiB in that minute, roughly
// 8.5 KiB/s — a link that is broken rather than merely slow. Even a failed layer
// being retried is not quiet: the backoff counts itself down with a "Retrying in
// N seconds" line once a second (distribution/xfer/download.go).
//
// What the value also has to clear is the windows where the daemon is WAITING on
// the registry rather than working, because nothing is written then:
//
//   - Between "Pulling fs layer" and the first byte of that blob. A pull-through
//     cache (Harbor, Nexus, an ECR pull-through rule) that has to fetch a cold
//     blob from upstream holds that window open for as long as the upstream
//     takes.
//   - Before the first progress line at all: "Pulling from <repo>" is written
//     only once the manifest has been fetched, so registry token auth and
//     manifest resolution happen in silence. That window sits INSIDE ImagePull
//     rather than after it, which is why Pull arms this bound before making that
//     call rather than once the progress stream is in hand — see the comment
//     there.
//
// A much shorter value would be wrong because of those windows as well as the
// cadence: at ten seconds a registry cold-starting a large blob is
// indistinguishable from a dead connection, and the pull would be cut in the one
// case where it was about to succeed. The costs are not symmetric either. A
// false abort fails a legitimate update, and it does so on exactly the images
// most likely to be slow to start; a stall noticed a minute later than it could
// have been costs one minute of an auto-update tick that the caller already
// bounds absolutely.
//
// This is a floor, not a ceiling: the caller keeps its own absolute bound (see
// defaultPullTimeout in api/docker, where a recreate gives its pull an hour), so
// a pull that dribbles bytes forever is still cut off eventually. The two answer
// different questions and neither replaces the other.
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
// A Puller built without NewPuller carries a zero timeout, and a zero idle
// deadline is one that has already expired: the abort would fire before the
// first chunk of the progress stream could arrive, so no pull could ever
// succeed. Falling back to the default keeps that from ever being a silent
// switch-off.
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
// It is a type rather than a few locals because the clock has to span two phases
// that live in different functions — the request Pull issues and the drain that
// follows it — and because the three things it carries (the timer, the duration
// it is rearmed with, and whether it has fired) are only ever correct together.
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
// The guard skips the rearm once the abort has already fired: that pull is dead
// and cannot be revived, and Reset on an AfterFunc timer that has already run
// schedules a fresh run of the function rather than rescheduling the old one. The
// check is advisory — the timer may fire between the load and the reset — so it
// only saves work rather than being load-bearing: abort is idempotent and stop
// takes any rearmed timer down.
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
	// The caller's own deadline or cancellation. Checked before the stall because
	// both end the read the same way, and because the difference matters to whoever
	// reads the log: "the recreate ran out of time" and "the pull went silent" call
	// for different things to look at.
	//
	// The wording says nothing about whose bound it was, deliberately. On the
	// manual recreate path the caller passes context.TODO() and has no deadline at
	// all; on the auto-update path there is one, but pullBudget shortens it, so what
	// fires is a bound Portainer computed rather than the caller's. The wrapped
	// error still distinguishes a cancellation from an expiry for anyone checking
	// with errors.Is.
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

	// The pull gets a cancellable context of its OWN, derived from the caller's,
	// and it is the context the request is issued on. That is what makes the
	// stall abort work at all: a blocked read of the response body unblocks when
	// the request's context is cancelled and in no other way, so cancelling
	// anything else — or merely returning early from the drain — would leave the
	// read parked on a dead connection until the caller's own deadline. Deriving
	// it from ctx keeps the caller's deadline and cancellation in force as well:
	// whichever of the two fires first ends the pull.
	pullCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// The idle clock is armed BEFORE the request goes out rather than once the
	// progress stream is in hand, because the longest guaranteed-silent phase of a
	// pull happens inside this call. ImagePull returns when the response HEADERS
	// arrive, and the daemon sends no header until its first write to the body — the
	// image route wraps the writer in a WriteFlusher it does not flush, precisely so
	// that a failure before the manifest can still be reported as an HTTP error —
	// while the first line it writes, "Pulling from <repo>", comes only once the
	// manifest has been fetched. Registry token auth and manifest resolution
	// therefore happen entirely in here, and a clock started after this call would
	// leave a pull wedged on the registry handshake hanging for the caller's whole
	// budget, an hour on the manual recreate path.
	//
	// The drain below takes the clock over and does the resetting from then on; it
	// is one clock across both phases, not two.
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
// The JSON decoder sits ON TOP of this rather than the other way round, and that
// order is load-bearing. Stall detection has to count BYTES, not messages: a
// connection that dies halfway through writing one progress line is as stalled as
// one that dies between two of them, and a clock reset on decoded messages would
// not notice until that line completed — which, for the line that never
// completes, is never.
//
// err is the reader's own account of how the stream ended, and it is what lets the
// drain tell a stream that FAILED from bytes the decoder could not make sense of,
// which are two things that reach the same branch and call for opposite responses.
// Only the draining goroutine ever touches it; the clock's own goroutine touches
// nothing here.
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

	// The reader's last word on how the stream ended — with one exception: a clean
	// io.EOF never erases a failure the reader has already reported. A reader is
	// allowed to hand out bytes and an error in the SAME call, and the decoder drops
	// the error whenever the read produced bytes, so such a failure reaches the drain
	// only through this field. Real bodies repeat it — net/http and gzip both keep
	// answering with the same error — but a reader that reports its failure once and
	// then ends politely would otherwise have that failure overwritten by the io.EOF
	// that followed, and the drain would call a broken download a finished pull. This
	// field is the reader's word on whether the end was CLEAN, and an end cannot
	// become clean after the fact.
	if err != nil && (stream.err == nil || !errors.Is(err, io.EOF)) {
		stream.err = err
	}

	// A read of no bytes and no error is legal and means, in io.Reader's own words,
	// that nothing happened. Going straight back for more would spin a core for as
	// long as such a reader keeps answering — up to a whole stall timeout, since the
	// idle clock is the only thing bounding the drain and it measures wall time, not
	// attempts — so an empty read is paid for with a brief wait rather than with a hot
	// retry. No net/http body behaves this way, but the drain does not get to choose
	// its reader.
	//
	// The pause lives HERE, in the one place every read passes through, rather than in
	// either loop above it. The fallback drain calls Read itself, but the ordinary path
	// does not: the decoder fills its buffer with io.ReadFull, which spins on a reader
	// like this exactly as eagerly and would never hand control back to a loop that
	// could pause on its behalf.
	if n == 0 && err == nil {
		time.Sleep(emptyReadBackoff)
	}

	return n, err
}

// drainProgress reads the pull's progress stream to its end, aborting the pull as
// soon as it has produced nothing for the guard's stall timeout, and failing it
// if the daemon reports a failure in band.
//
// It replaces an io.ReadAll, which discarded the same bytes but only after
// buffering every one of them and only after the stream had already ended — which
// is to say it threw away both signals a pull's progress stream carries: whether
// it is still alive, and whether it worked.
//
// ctx is the CALLER's context, not the pull's; see stallGuard.outcome for why
// that distinction is the only way the outcomes can be told apart.
func drainProgress(ctx context.Context, progress io.Reader, guard *stallGuard) error {
	stream := &progressStream{inner: progress, guard: guard}

	// The decoder keeps ONE buffer and compacts what it has consumed before reading
	// more, so memory here is bounded rather than growing with the length of the
	// stream, which is the property io.ReadAll lacked and the reason it was dropped.
	// The bound is roughly the larger of 32 KiB and twice the longest single message:
	// the buffer starts at minBufferSize (32 KiB) and doubles whenever the free space
	// left in it drops below minReadSize (4 KiB).
	//
	// Bounded is not the same as line at a time, and the difference is worth writing
	// down because the code reads as though Decode returned on the first newline. It
	// does not: the decoder fills its buffer with io.ReadFull, which keeps reading
	// until it has those 32 KiB or the reader errors, so one Decode call absorbs
	// however many messages the daemon has already written. Nothing here depends on
	// that — moby closes the stream straight after writing an errorDetail message, so
	// there is never a queue of lines hiding behind a reported failure — but it does
	// mean this loop acts on a failure once the free space in that buffer has been
	// filled or the reader has failed or ended, rather than at the instant the line
	// lands on the wire. A pause in the stream is not one of those moments: io.ReadFull
	// simply keeps waiting through it.
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

		// The stream ran to its end, which is how a finished pull ends — but the READER
		// has the last word on whether that end was clean, and an io.EOF out of the
		// decoder is not by itself proof that it was.
		//
		// The decoder fills its buffer with io.ReadFull, and a read that returns no
		// bytes and io.ErrUnexpectedEOF — precisely what net/http's chunked reader
		// gives for a response body cut short — is rewritten to a plain io.EOF on its
		// way out of readValue. The daemon flushes each progress line as its own chunk,
		// so a connection dropped mid-download most often breaks on a message boundary,
		// and a boundary cut arrives here indistinguishable from a pull that finished:
		// nothing half-parsed is left in the decoder to give it away. Taking that at
		// face value would report a truncated download as an updated image, which is the
		// failure this drain exists to stop, on the pulls most exposed to it — the long
		// ones, where a proxy, an agent tunnel or a restarted daemon has the most time
		// to close the connection.
		//
		// So the success needs the reader to agree: no error at all, or an io.EOF of its
		// own. Checking the reader's error rather than the caller's context also keeps
		// the property that a cancellation landing in the same instant as the last byte
		// still yields the pull that was asked for.
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

		// The decoder's OWN verdict that the stream stopped in the middle of a value.
		// That is the other shape a download cut short takes, and the one no reader error
		// gives away: readValue returns io.ErrUnexpectedEOF when its buffer still holds an
		// unterminated but so-far-valid JSON prefix and the reader has nothing left to add
		// — INCLUDING when the reader ended perfectly cleanly, which is what a proxy, an
		// agent tunnel or a restarted daemon closing the connection half a line in looks
		// like from here. Neither check above catches that combination: the decoder's error
		// is not an io.EOF, and the reader's last word was one, so comparing the two says
		// nothing. Without this the pull would degrade to the byte drain, meet the reader's
		// clean io.EOF a moment later and be reported as an updated image.
		//
		// Acting on it cannot invent a failure the pull never had. Bytes that are not JSON
		// at all — a stream framed some way this code does not know — fail as a
		// *json.SyntaxError and still take the forgiving path below; io.ErrUnexpectedEOF is
		// returned only for input that was still VALID when it ran out, which is truncation
		// by definition, and a truncated progress stream is a pull whose image never
		// finished arriving however politely the connection was closed.
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return guard.outcome(ctx, err)
		}

		// A message that PARSED but did not fit — a field carrying a type this struct
		// does not declare — costs nothing and is skipped. The value was consumed whole
		// before the mismatch was reported, so the decoder is still sitting on a message
		// boundary and every line after it reads normally; giving up on the rest of the
		// stream over one odd message would let a failure reported ten lines later go
		// unnoticed for no gain. The half-filled struct is dropped rather than checked,
		// because a message the decoder could not finish is not evidence that anything
		// failed.
		//
		// A syntax error is the opposite case and is not recoverable in the same way:
		// the value did not parse, so the decoder cannot tell where the next one starts,
		// and there is nothing left to do but stop reading messages. That is what the
		// fallback below is for.
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

		// Everything else is the decoder failing to make sense of bytes that did
		// arrive — a malformed line, a shape nobody expected, a stream framed some way
		// this code does not know about — and it degrades to exactly what this function
		// did before it could parse anything: drain the rest and call EOF a success.
		//
		// That degradation is the whole safety property of parsing here. Reading the
		// stream may only ever turn a failure that was SILENT into a loud one; it must
		// never invent a failure that did not exist. A pull the daemon completed
		// successfully must not start failing because Portainer could not read a line
		// of its commentary, so a parse problem is a warning in the log and nothing
		// more.
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
// stream. Download attempts exhausted, a layer digest mismatch, no space left on
// device during extraction, a registry 5xx, an unexpected EOF mid-blob — all of
// them arrive this way, and all of them used to be discarded.
//
// Discarding them was worse than losing an error message. Recreate goes on to
// build the new container from the image NAME, and on an auto-update that tag
// always resolves locally to the digest that is already there, so the workload
// would be stopped, renamed, recreated FROM THE STALE IMAGE and started again,
// with the recreate reporting success and nothing updated — and the next status
// check would find it outdated and do it all over again, a restart loop that
// never converges and never logs a reason.
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

// drainBytes reads what is left of the stream and throws it away, which is what
// the drain did in full before it learned to read the messages. It keeps the idle
// clock running — the reader it is given is the one that resets it — so a stream
// that stops making sense and then goes silent is still abandoned as a stall
// rather than waited on.
//
// Unlike the decoding loop above, this one may take io.EOF at face value: it reads
// the stream directly, so the reader's error arrives here as the reader gave it,
// and a body cut short still says io.ErrUnexpectedEOF rather than having it
// rewritten to an io.EOF by a decoder in between.
func drainBytes(ctx context.Context, progress io.Reader, guard *stallGuard) error {
	buf := make([]byte, progressBufferSize)

	for {
		// The idle clock and the caller are asked directly, before every read, because
		// neither of them can end this loop through the reader alone. Both of them only
		// cancel a context: the clock cancels the pull's, the caller cancels its own, and
		// a cancelled context unblocks a net/http body but means nothing at all to a
		// reader that never consults one — the same reader the pause on empty reads exists
		// for. Asking here is what makes "a stream that goes quiet is abandoned as a
		// stall" a property of this drain rather than one borrowed from whatever reader
		// happened to turn up.
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
