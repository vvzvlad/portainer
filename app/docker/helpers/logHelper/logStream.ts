import { formatLogs } from './formatLogs';
import { FormattedLine } from './types';

type ProcessorOptions = {
  stripHeaders?: boolean;
  withTimestamps?: boolean;
};

/**
 * Stateful processor for an incremental (HTTP-streamed) container log response.
 *
 * A streaming response arrives in arbitrary byte/text chunks that split log
 * lines at random points, and — for non-TTY containers — every line is framed
 * with Docker's 8-byte multiplexed-stream header. This processor:
 *
 *  - buffers the incoming text and only ever emits **complete** lines (cutting
 *    on the last newline), carrying the trailing partial line forward;
 *  - feeds each completed batch through the existing `formatLogs` (which strips
 *    the 8-byte headers via `stripHeadersFunc`, unwraps JSON/zerolog and applies
 *    ANSI colours) — no bespoke demux logic, per the issue;
 *  - relies on the invariant that every cut happens on a newline boundary, so
 *    each batch starts exactly on a frame header boundary and `stripHeadersFunc`
 *    (`substring(8)` + strip-after-newline) demuxes it correctly.
 *
 * Each emitted line carries a stable, monotonically increasing `id`, so the
 * viewer can `track by log.id` and append without re-binding existing rows.
 */
export function createLogStreamProcessor({
  stripHeaders,
  withTimestamps,
}: ProcessorOptions = {}) {
  let buffer = '';

  return {
    /**
     * Append a decoded text chunk and return the newly completed, formatted
     * lines. Returns an empty array when the chunk did not complete any line.
     */
    push(chunk: string): FormattedLine[] {
      buffer += chunk;

      const lastNewline = buffer.lastIndexOf('\n');
      if (lastNewline === -1) {
        // no complete line yet, keep buffering
        return [];
      }

      const completed = buffer.slice(0, lastNewline + 1);
      buffer = buffer.slice(lastNewline + 1);

      return formatLogs(completed, { stripHeaders, withTimestamps });
    },

    /**
     * Flush any buffered partial line (e.g. when the stream ends without a
     * trailing newline). Returns the formatted remainder, if any.
     */
    flush(): FormattedLine[] {
      if (!buffer) {
        return [];
      }
      const remainder = buffer;
      buffer = '';
      return formatLogs(remainder, { stripHeaders, withTimestamps });
    },
  };
}

export type LogStreamProcessor = ReturnType<typeof createLogStreamProcessor>;
