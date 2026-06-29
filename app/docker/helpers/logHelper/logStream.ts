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
   * re-delivers the boundary line(s). Drop re-delivered lines whose timestamp is
   * `<=` this value (the resume point) until we pass it.
   */
  skipUntilTimestamp?: string;
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
}: ProcessorOptions = {}) {
  // Unparsed bytes of the multiplexed frame stream (non-TTY only).
  let frameBuf: Bytes = new Uint8Array(0);
  // Decoded-payload bytes not yet terminated by a newline.
  let lineBuf: Bytes = new Uint8Array(0);
  // Reconnect dedup: while true, drop re-delivered lines up to the resume point.
  let skipping = !!skipUntilTimestamp;
  // RFC3339 timestamp of the last complete line we saw (next resume point).
  let lastTimestamp: string | undefined;

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
        // Corrupt/desynced stream: drop everything buffered to avoid OOM.
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
        // Drop re-delivered lines whose fixed-width RFC3339 timestamp is `<=`
        // the resume point (lexical compare is chronological for the zero-padded
        // UTC format). A genuinely new line sharing the exact same nanosecond
        // timestamp is an accepted rare edge.
        let dropTo = 0;
        while (dropTo < lines.length) {
          const line = lines[dropTo];
          if (
            TS_PREFIX.test(line) &&
            line.substring(0, TS_WIDTH) <= (skipUntilTimestamp as string)
          ) {
            dropTo += 1;
          } else {
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

      // Record the resume timestamp from the last timestamped line (still has
      // its prefix at this point).
      for (let i = lines.length - 1; i >= 0; i -= 1) {
        if (TS_PREFIX.test(lines[i])) {
          lastTimestamp = lines[i].substring(0, TS_WIDTH);
          break;
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
  };
}

export type LogStreamProcessor = ReturnType<typeof createLogStreamProcessor>;
