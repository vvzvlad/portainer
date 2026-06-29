import { formatLogs } from './formatLogs';
import { FormattedLine, TIMESTAMP_LENGTH } from './types';

type ProcessorOptions = {
  /** Non-TTY container: demux Docker's 8-byte multiplexed-stream frame headers. */
  stripHeaders?: boolean;
  /** Display the RFC3339 timestamp prefix (user's "Display timestamps" toggle). */
  withTimestamps?: boolean;
  /**
   * The raw stream always carries an RFC3339 timestamp prefix because the
   * controller requests `timestamps=1` internally (so it can resume from the
   * exact log timestamp on reconnect). When `withTimestamps` is false we strip
   * the prefix before formatting so it is not displayed.
   */
  streamHasTimestamps?: boolean;
  /**
   * Reconnect dedup: Docker's `since` filter is inclusive, so on reconnect it
   * re-delivers the boundary line(s) at the resume timestamp. This is the resume
   * point (the RFC3339 timestamp of the last line we saw). Re-delivered lines
   * before it are dropped; lines exactly at it are matched against
   * `skipBoundaryContents` (see below).
   */
  skipUntilTimestamp?: string;
  /**
   * Reconnect dedup, exact-content matching. The lines we had already shown that
   * carry the resume timestamp (`skipUntilTimestamp`). On reconnect Docker
   * re-delivers every line at that timestamp; we drop only those whose exact
   * content matches one of these (the genuine duplicates) and KEEP any line that
   * merely shares the timestamp but is new (different content) — otherwise a
   * second line sharing the boundary nanosecond would be lost.
   */
  skipBoundaryContents?: string[];
};

// Docker multiplexed-stream frame header: 1 byte stream-type, 3 zero bytes,
// then a 4-byte big-endian payload length, followed by exactly `length` payload
// bytes. See the Docker Engine API container "logs" (non-TTY) documentation.
const FRAME_HEADER_SIZE = 8;
const FRAME_LENGTH_OFFSET = 4;

// Defensive only: a single frame payload larger than this almost certainly means
// the byte stream desynced/corrupted. This is NOT a cap on legitimate lines
// (real log lines are orders of magnitude smaller); it only prevents unbounded
// buffering if we ever lose frame alignment.
const MAX_FRAME_PAYLOAD = 100 * 1024 * 1024;

// Width of Docker's fixed RFC3339Nano timestamp prefix emitted with
// `timestamps=1`: "2006-01-02T15:04:05.000000000Z" — 30 chars; index 30 is the
// separating space (TIMESTAMP_LENGTH = 31 = 30 + space).
const TS_WIDTH = TIMESTAMP_LENGTH - 1;
const TS_PREFIX = /^\d{4}-\d{2}-\d{2}T/;

const LF = 0x0a;
const CR = 0x0d;

// `subarray()` widens the backing buffer to ArrayBufferLike under TS 5.7+; use a
// permissive byte-array alias so buffered slices assign cleanly.
type Bytes = Uint8Array<ArrayBufferLike>;

function concatBytes(a: Bytes, b: Bytes): Bytes {
  if (a.length === 0) {
    return b;
  }
  if (b.length === 0) {
    return a;
  }
  const out = new Uint8Array(a.length + b.length);
  out.set(a, 0);
  out.set(b, a.length);
  return out;
}

function readUint32BE(buf: Bytes, offset: number): number {
  // `* 0x1000000` instead of `<< 24` to keep the high byte unsigned.
  return (
    buf[offset] * 0x1000000 +
    (buf[offset + 1] << 16) +
    (buf[offset + 2] << 8) +
    buf[offset + 3]
  );
}

/**
 * Stateful processor for an incremental (HTTP-streamed) container log response.
 *
 * A streaming response arrives in arbitrary byte chunks that split frames, log
 * lines and even multibyte UTF-8 characters at random points. For non-TTY
 * containers the body is Docker's binary multiplexed stream: each log message is
 * framed with an 8-byte header carrying the payload length. This processor
 * demuxes at the BYTE level — never on `\n` and never by UTF-8-decoding header
 * bytes — because the low byte of a frame's length field can itself be `0x0a`
 * and header bytes can be `>= 0x80` (both would corrupt a text-level demux):
 *
 *  - it buffers raw bytes and parses complete frames by their declared length,
 *    carrying any partial trailing frame forward;
 *  - it concatenates payload bytes across frames, splits them into complete
 *    lines on the `0x0a` byte, and UTF-8-decodes each complete line as a whole
 *    (so a multibyte char split across frames/chunks is already reassembled);
 *  - because headers are removed here, the completed text lines are formatted by
 *    `formatLogs` WITHOUT its `stripHeaders` option (otherwise it would strip 8
 *    more characters of real content).
 *
 * Each emitted line carries a stable, monotonically increasing `id`, so the
 * viewer can `track by log.id` and append without re-binding existing rows.
 */
export function createLogStreamProcessor({
  stripHeaders,
  withTimestamps,
  streamHasTimestamps,
  skipUntilTimestamp,
  skipBoundaryContents,
}: ProcessorOptions = {}) {
  // Unparsed bytes of the multiplexed frame stream (non-TTY only).
  let frameBuf: Bytes = new Uint8Array(0);
  // Decoded-payload bytes not yet terminated by a newline.
  let lineBuf: Bytes = new Uint8Array(0);
  // Reconnect dedup: while true, drop re-delivered lines up to the resume point.
  let skipping = !!skipUntilTimestamp;
  // Copy of the boundary line contents still pending a duplicate match; each
  // matched re-delivered line consumes (splices out) one entry.
  const pendingBoundary: string[] = skipBoundaryContents
    ? skipBoundaryContents.slice()
    : [];
  // RFC3339 timestamp of the last complete line we saw (next resume point).
  // Seeded from the resume point so the boundary set carried in via
  // `skipBoundaryContents` is preserved: a surviving line at the resume
  // timestamp is APPENDED to the already-known boundary lines (not treated as a
  // fresh timestamp), so a second reconnect at the same nanosecond still knows
  // every line we have shown at it and drops them all instead of re-emitting
  // duplicates. The first NEWER timestamp resets the set as usual.
  let lastTimestamp: string | undefined = skipUntilTimestamp;
  // Exact content of the line(s) at `lastTimestamp` we have emitted so far — the
  // boundary set handed to the next processor for content-exact reconnect dedup.
  // Seeded from the inbound boundary set (see `lastTimestamp` above).
  let boundaryLines: string[] = skipBoundaryContents
    ? skipBoundaryContents.slice()
    : [];

  const decoder = new TextDecoder();

  // Extract complete newline-terminated lines from the accumulated payload
  // bytes, decoding each whole line at once. A `0x0a` byte can never be part of
  // a multibyte UTF-8 sequence, so a line boundary never cuts a character.
  function takeCompleteLines(payload: Bytes): string[] {
    lineBuf = concatBytes(lineBuf, payload);
    const lines: string[] = [];
    let start = 0;
    for (let i = 0; i < lineBuf.length; i += 1) {
      if (lineBuf[i] === LF) {
        let end = i;
        // drop a trailing CR for \r\n line endings
        if (end > start && lineBuf[end - 1] === CR) {
          end -= 1;
        }
        lines.push(decoder.decode(lineBuf.subarray(start, end)));
        start = i + 1;
      }
    }
    lineBuf = lineBuf.subarray(start);
    return lines;
  }

  // Pull every complete frame payload out of frameBuf and return the decoded
  // complete lines they contain. An incomplete trailing frame stays buffered.
  function demuxFrames(): string[] {
    const lines: string[] = [];
    for (;;) {
      if (frameBuf.length < FRAME_HEADER_SIZE) {
        break;
      }
      const payloadLength = readUint32BE(frameBuf, FRAME_LENGTH_OFFSET);
      if (payloadLength > MAX_FRAME_PAYLOAD) {
        // Corrupt/desynced stream: drop everything buffered to avoid OOM. Surface
        // it so an unexpected reset is diagnosable rather than silently swallowed.
        // eslint-disable-next-line no-console
        console.warn(
          `logStream: frame payload length ${payloadLength} exceeds ${MAX_FRAME_PAYLOAD}; resetting desynced buffer`
        );
        frameBuf = new Uint8Array(0);
        break;
      }
      if (frameBuf.length - FRAME_HEADER_SIZE < payloadLength) {
        break; // wait for the rest of the payload
      }
      const payload = frameBuf.subarray(
        FRAME_HEADER_SIZE,
        FRAME_HEADER_SIZE + payloadLength
      );
      lines.push(...takeCompleteLines(payload));
      frameBuf = frameBuf.subarray(FRAME_HEADER_SIZE + payloadLength);
    }
    return lines;
  }

  // Apply reconnect-dedup + timestamp handling, then format a batch of complete
  // text lines into rendered lines.
  function formatBatch(rawLines: string[]): FormattedLine[] {
    let lines = rawLines;

    if (streamHasTimestamps) {
      if (skipping && lines.length) {
        // Drop re-delivered lines up to the resume point. Lines strictly before
        // it are duplicates (lexical compare is chronological for the zero-padded
        // UTC format). Lines exactly at the resume timestamp are dropped ONLY
        // when their exact content matches a boundary line we already showed —
        // so a NEW line sharing the same nanosecond timestamp is not lost.
        const boundary = skipUntilTimestamp as string;
        let dropTo = 0;
        while (dropTo < lines.length) {
          const line = lines[dropTo];
          if (!TS_PREFIX.test(line)) {
            break; // cannot classify without a timestamp; stop dropping
          }
          const ts = line.substring(0, TS_WIDTH);
          const boundaryIdx =
            ts === boundary ? pendingBoundary.indexOf(line) : -1;
          if (ts < boundary) {
            // re-delivered line before the resume point
            dropTo += 1;
          } else if (boundaryIdx !== -1) {
            // exact duplicate of an already-shown boundary line
            pendingBoundary.splice(boundaryIdx, 1);
            dropTo += 1;
          } else {
            // ts > boundary, or a new line that merely shares the boundary
            // timestamp -> keep it and everything after.
            break;
          }
        }
        if (dropTo > 0) {
          lines = lines.slice(dropTo);
        }
        if (lines.length) {
          skipping = false;
        }
      }

      // Track the resume timestamp and the exact content of the line(s) at it,
      // walking the kept lines in order (each line still has its prefix here).
      // Timestamps are monotonically non-decreasing, so the trailing run sharing
      // the newest timestamp is the boundary set the next processor dedups on.
      for (let i = 0; i < lines.length; i += 1) {
        const line = lines[i];
        if (TS_PREFIX.test(line)) {
          const ts = line.substring(0, TS_WIDTH);
          if (ts !== lastTimestamp) {
            lastTimestamp = ts;
            boundaryLines = [line];
          } else {
            boundaryLines.push(line);
          }
        }
      }

      // When the user has timestamps hidden, strip the prefix we requested
      // internally so it is not displayed.
      if (!withTimestamps) {
        lines = lines.map((line) =>
          TS_PREFIX.test(line) ? line.substring(TIMESTAMP_LENGTH) : line
        );
      }
    }

    if (!lines.length) {
      return [];
    }

    // Rejoin into a single batch so ANSI colour state carries across lines
    // within the batch (matches the previous whole-batch formatLogs call).
    // Headers are already stripped at the byte layer, so do NOT pass
    // stripHeaders here.
    const batch = `${lines.join('\n')}\n`;
    return formatLogs(batch, { withTimestamps });
  }

  return {
    /**
     * Append a raw byte chunk and return the newly completed, formatted lines.
     * Returns an empty array when the chunk did not complete any line.
     */
    push(value: Bytes): FormattedLine[] {
      if (!value || value.length === 0) {
        return [];
      }
      let rawLines: string[];
      if (stripHeaders) {
        frameBuf = concatBytes(frameBuf, value);
        rawLines = demuxFrames();
      } else {
        rawLines = takeCompleteLines(value);
      }
      return formatBatch(rawLines);
    },

    /**
     * Flush any buffered partial line (e.g. when the stream ends without a
     * trailing newline). A partial/truncated final frame is dropped so we never
     * emit garbage.
     */
    flush(): FormattedLine[] {
      // Drop any incomplete trailing frame.
      frameBuf = new Uint8Array(0);
      if (lineBuf.length === 0) {
        return [];
      }
      let bytes: Bytes = lineBuf;
      // drop a trailing CR for \r\n line endings
      if (bytes.length > 0 && bytes[bytes.length - 1] === CR) {
        bytes = bytes.subarray(0, bytes.length - 1);
      }
      lineBuf = new Uint8Array(0);
      return formatBatch([decoder.decode(bytes)]);
    },

    /** RFC3339 timestamp of the last complete line seen, for reconnect resume. */
    getLastTimestamp(): string | undefined {
      return lastTimestamp;
    },

    /**
     * Exact content of the line(s) carrying the last-seen timestamp. Pass to the
     * next processor as `skipBoundaryContents` so reconnect dedup drops only the
     * genuine duplicates Docker re-delivers, never a new line that happens to
     * share the boundary nanosecond.
     */
    getBoundaryLines(): string[] {
      return boundaryLines.slice();
    },
  };
}

export type LogStreamProcessor = ReturnType<typeof createLogStreamProcessor>;
