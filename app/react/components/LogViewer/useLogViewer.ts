import { useEffect, useRef, useState } from 'react';

import {
  createLogStreamProcessor,
  rfc3339ToUnixNanoSince,
} from '@/docker/helpers/logHelper';
import { FormattedLine } from '@/docker/helpers/logHelper/types';
import { StreamLogsParams } from '@/react/docker/containers/containers.service';

import { StreamLogsFn } from './types';

// Hard cap on how many lines we keep in memory/DOM during a long live stream.
// We trim from the head (oldest lines) so a selection anchored in the tail is
// never disturbed. Mirrors the legacy AngularJS controller. There is no
// windowing/virtualisation dependency available in this repo, so this cap plus
// the browser's native scroll is what keeps a chatty container from locking the
// UI; the user-facing "Lines" input is a historical-backfill *request*, not this
// in-memory limit.
const MAX_LOG_LINES = 5000;
// Delay before reconnecting after a following stream ends or errors (container
// stopped, network blip, proxy hiccup). Avoids hammering a stopped container.
const RECONNECT_DELAY_MS = 3000;
// Default tail (historical backfill) used when the "Lines" field is cleared or
// invalid — without it Docker re-downloads the entire history of a chatty
// container on every parameter change.
const DEFAULT_LINE_COUNT = 100;
// Coalesce incoming lines and repaint at most this often. A busy container can
// deliver thousands of lines/sec; flushing to React state on every chunk would
// thrash rendering, so we buffer and flush on an interval instead.
const FLUSH_INTERVAL_MS = 250;

export interface UseLogViewerOptions {
  streamLogs: StreamLogsFn;
  /**
   * Identifies the resource whose logs are streamed (e.g. the container id).
   * `streamLogs` closes over it, so it is a dependency of the stream even though
   * it is not passed in the params: changing it restarts the stream from scratch
   * (fresh buffer, no cross-resource `since` resume), independent of whether the
   * host directive happens to remount the component.
   */
  resourceId: string;
  /** Non-TTY container: demux Docker's 8-byte multiplexed-stream frame headers. */
  stripHeaders: boolean;
  /** ON = keep a following stream open; OFF = fetch one bounded snapshot. */
  autoRefresh: boolean;
  /**
   * User's "Timestamp" toggle — display the RFC3339 prefix on each line.
   * NOTE (known follow-up): toggling this currently restarts the stream (it is
   * an effect dependency), matching the legacy AngularJS controller which also
   * re-requested on a `displayTimestamps` change. Re-formatting the existing
   * buffer in place instead (the prefix is already parsed by the processor)
   * would avoid the refetch but is a larger change; deferred for parity.
   */
  withTimestamps: boolean;
  /** Historical-backfill tail request ("Lines" input). */
  lineCount: number;
  /** Lower bound of the snapshot window (from the datetime range picker). */
  since: Date | null;
  /** Upper bound of the snapshot window (from the datetime range picker). */
  until: Date | null;
  /** Bump to force a fresh reconnect/refetch (the reload icon). */
  reloadNonce: number;
}

export interface UseLogViewerResult {
  logs: FormattedLine[];
  error?: string;
}

function toUnixSeconds(date: Date | null): number | undefined {
  return date ? Math.floor(date.getTime() / 1000) : undefined;
}

/**
 * Drives the log data for the viewer. This is a faithful port of the legacy
 * `containerLogsController.js` streaming logic (byte-level demux via
 * `createLogStreamProcessor`, `timestamps=1` always requested internally so a
 * reconnect can resume from the exact log timestamp, reconnect dedup on the
 * boundary line, abort on unmount) with two additions:
 *
 *  - `autoRefresh` off fetches a single bounded snapshot (`follow=0`) for the
 *    selected range/lines instead of following, and does not reconnect;
 *  - a `since`/`until` window (from the datetime range picker) bounds that
 *    snapshot.
 *
 * Presentation state (search, toggles, filtering) lives in the component; this
 * hook only owns the line buffer and the network lifecycle.
 */
export function useLogViewer({
  streamLogs,
  resourceId,
  stripHeaders,
  autoRefresh,
  withTimestamps,
  lineCount,
  since,
  until,
  reloadNonce,
}: UseLogViewerOptions): UseLogViewerResult {
  const [logs, setLogs] = useState<FormattedLine[]>([]);
  const [error, setError] = useState<string>();

  // Keep the latest streamLogs identity in a ref so it does not have to be a
  // useEffect dependency: callers typically pass a freshly-bound function on
  // every render, which would otherwise restart the stream on each repaint.
  const streamLogsRef = useRef(streamLogs);
  useEffect(() => {
    streamLogsRef.current = streamLogs;
  });

  const sinceSeconds = toUnixSeconds(since);
  const untilSeconds = toUnixSeconds(until);

  useEffect(() => {
    let active = true;
    let abortController: AbortController | null = null;
    let reconnectTimer: ReturnType<typeof setTimeout> | null = null;
    let flushTimer: ReturnType<typeof setTimeout> | null = null;
    // RFC3339 timestamp of the last line seen + the exact content of the line(s)
    // at it, for content-exact reconnect dedup (see logStream.ts).
    let lastTimestamp = '';
    let boundaryLines: string[] = [];
    // Suppress duplicate error surfacing across the 3s reconnect loop.
    let errorNotified = false;
    // Buffer for coalesced flushing.
    let pending: FormattedLine[] = [];

    // Intentional reset when a fetch parameter changes: drop the previous
    // request's buffer/error so the new stream starts from a clean slate
    // (mirrors the legacy controller clearing $scope.logs on a param change).
    // eslint-disable-next-line react-hooks/set-state-in-effect
    setLogs([]);
    setError(undefined);

    function flush() {
      flushTimer = null;
      if (!active || pending.length === 0) {
        return;
      }
      const batch = pending;
      pending = [];
      setLogs((prev) => {
        const next = prev.concat(batch);
        const overflow = next.length - MAX_LOG_LINES;
        // trim oldest lines; the tail (and any selection in it) is untouched
        return overflow > 0 ? next.slice(overflow) : next;
      });
    }

    function scheduleFlush() {
      if (flushTimer === null) {
        flushTimer = setTimeout(flush, FLUSH_INTERVAL_MS);
      }
    }

    function appendLines(lines: FormattedLine[]) {
      if (!lines.length) {
        return;
      }
      pending = pending.concat(lines);
      scheduleFlush();
    }

    async function connect() {
      if (!active) {
        return;
      }
      const resuming = !!lastTimestamp;

      const processor = createLogStreamProcessor({
        stripHeaders,
        withTimestamps,
        // Always request timestamps from Docker so a reconnect can resume from
        // the exact log position; the processor strips the prefix for display
        // when the user's Timestamp toggle is off.
        streamHasTimestamps: true,
        skipUntilTimestamp: resuming ? lastTimestamp : undefined,
        skipBoundaryContents: resuming ? boundaryLines : undefined,
      });

      abortController = new AbortController();
      const { signal } = abortController;

      // Resume from the exact last-seen timestamp on reconnect; otherwise honour
      // the picker's "from" bound. Both are sent as "<unix>.<nanos>".
      let sinceParam: number | string | undefined;
      if (lastTimestamp) {
        sinceParam = rfc3339ToUnixNanoSince(lastTimestamp);
      } else if (sinceSeconds !== undefined) {
        sinceParam = `${sinceSeconds}.000000000`;
      } else {
        sinceParam = undefined; // omitted -> Docker tails from now
      }

      const tail = lineCount > 0 ? lineCount : DEFAULT_LINE_COUNT;

      const params: StreamLogsParams = {
        stdout: true,
        stderr: true,
        follow: autoRefresh,
        timestamps: true,
        // tail is a historical-backfill request: apply it only on the initial
        // connect. On reconnect we resume from `since`, so re-applying tail would
        // re-deliver the tail window. (0 is dropped by the service.)
        tail: resuming ? 0 : tail,
        since: sinceParam,
        // `until` only bounds a static snapshot; a following stream never sets it.
        until: !autoRefresh ? untilSeconds : undefined,
      };

      try {
        await streamLogsRef.current(
          params,
          (bytes) => {
            const lines = processor.push(bytes);
            appendLines(lines);
            const ts = processor.getLastTimestamp();
            if (ts) {
              lastTimestamp = ts;
              boundaryLines = processor.getBoundaryLines();
            }
            if (lines.length) {
              // Logs are flowing again: allow a fresh error next failure and
              // clear any stale error banner left over from a previous drop so
              // it does not linger after the stream recovers on reconnect.
              errorNotified = false;
              setError(undefined);
            }
          },
          signal,
          () => {
            // Stream opened successfully (headers received, HTTP ok). Clear any
            // stale error banner from a previous drop even when the container is
            // idle and no new line arrives, so a healthy reconnect does not leave
            // the banner stuck. Only fires on a real open, so a failing reconnect
            // (which rejects before this) still surfaces its error.
            if (!active) {
              return;
            }
            errorNotified = false;
            setError(undefined);
          }
        );
        if (!active) {
          return;
        }
        if (autoRefresh) {
          // Following: discard the processor's unfinished trailing remainder and
          // reconnect from the last complete line (see the legacy controller —
          // flushing a truncated fragment would be re-sent under `since` and
          // dedup-dropped, losing the line).
          scheduleReconnect();
        } else {
          // Snapshot: the body ended, so flush the final partial line and stop.
          appendLines(processor.flush());
        }
      } catch (err) {
        if (signal.aborted || !active) {
          return; // intentional abort (unmount / param change)
        }
        if (!errorNotified) {
          errorNotified = true;
          setError((err as Error).message || 'Unable to stream logs');
        }
        if (autoRefresh) {
          scheduleReconnect();
        }
      }
    }

    function scheduleReconnect() {
      if (!active) {
        return;
      }
      if (reconnectTimer) {
        clearTimeout(reconnectTimer);
      }
      reconnectTimer = setTimeout(() => {
        if (active) {
          void connect();
        }
      }, RECONNECT_DELAY_MS);
    }

    void connect();

    return () => {
      active = false;
      if (reconnectTimer) {
        clearTimeout(reconnectTimer);
      }
      if (flushTimer) {
        clearTimeout(flushTimer);
      }
      if (abortController) {
        abortController.abort();
      }
    };
    // resourceId/stripHeaders/withTimestamps/autoRefresh/lineCount/since/until/
    // reloadNonce each restart the stream from scratch (fresh request + buffer),
    // mirroring the legacy `$watch`-driven restarts. resourceId is included so a
    // container switch resets the buffer and resume point without relying on the
    // host directive remounting the component.
  }, [
    resourceId,
    stripHeaders,
    withTimestamps,
    autoRefresh,
    lineCount,
    sinceSeconds,
    untilSeconds,
    reloadNonce,
  ]);

  return { logs, error };
}
