import { waitFor } from '@testing-library/react';
import { act, renderHook } from '@testing-library/react-hooks';

import { StreamLogsParams } from '@/react/docker/containers/containers.service';
import { rfc3339ToUnixNanoSince } from '@/docker/helpers/logHelper';

import { StreamLogsFn } from './types';
import { useLogViewer, UseLogViewerOptions } from './useLogViewer';

const encoder = new TextEncoder();

// Docker's fixed-width RFC3339Nano prefix ("2006-01-02T15:04:05.000000000Z ") the
// hook always requests internally (timestamps=1); the processor uses it to resume
// and dedup on reconnect. Build a complete, newline-terminated timestamped line.
function tsLine(seconds: string, text: string): string {
  return `2020-01-01T00:00:0${seconds}.000000000Z ${text}\n`;
}

// Baseline options so each test only overrides what it exercises.
const baseOptions: Omit<UseLogViewerOptions, 'streamLogs'> = {
  resourceId: 'container-1',
  stripHeaders: false,
  autoRefresh: false,
  withTimestamps: false,
  lineCount: 100,
  since: null,
  until: null,
  reloadNonce: 0,
};

describe('useLogViewer', () => {
  it('fetches a single bounded snapshot (no follow, no reconnect) when auto refresh is off', async () => {
    let callCount = 0;
    let capturedParams: StreamLogsParams | undefined;

    // A snapshot source: emits two complete lines plus a trailing partial line
    // (no newline) then resolves — mirroring a non-following body that ends.
    // eslint-disable-next-line func-style
    const streamLogs: StreamLogsFn = (params, onChunk) => {
      callCount += 1;
      capturedParams = params;
      onChunk(encoder.encode('first\nsecond\ntrailing-partial'));
      return Promise.resolve();
    };

    const { result } = renderHook(() =>
      useLogViewer({
        streamLogs,
        resourceId: 'container-1',
        stripHeaders: false,
        autoRefresh: false,
        withTimestamps: false,
        lineCount: 100,
        since: new Date('2020-01-01T00:00:00Z'),
        until: new Date('2020-01-02T00:00:00Z'),
        reloadNonce: 0,
      })
    );

    // The trailing partial line is flushed when the body ends, so all three
    // lines are shown.
    await waitFor(() => {
      expect(result.current.logs).toHaveLength(3);
    });
    expect(result.current.logs.map((l) => l.line)).toEqual([
      'first',
      'second',
      'trailing-partial',
    ]);

    // follow off, until bound applied, and no reconnect (called exactly once).
    expect(capturedParams?.follow).toBe(false);
    expect(capturedParams?.until).toBe(
      Math.floor(new Date('2020-01-02T00:00:00Z').getTime() / 1000)
    );
    expect(callCount).toBe(1);
  });

  it('clears the error banner when a following stream recovers on reconnect', async () => {
    vi.useFakeTimers();
    try {
      let call = 0;
      // First connect rejects (surfaces the banner); the reconnect succeeds and
      // delivers a line (should clear the banner).
      // eslint-disable-next-line func-style
      const streamLogs: StreamLogsFn = (_params, onChunk) => {
        call += 1;
        if (call === 1) {
          return Promise.reject(new Error('boom'));
        }
        onChunk(encoder.encode('recovered\n'));
        // stay open so no further reconnect fires during the test
        return new Promise(() => {});
      };

      const { result } = renderHook(() =>
        useLogViewer({
          streamLogs,
          resourceId: 'container-1',
          stripHeaders: false,
          autoRefresh: true,
          withTimestamps: false,
          lineCount: 100,
          since: null,
          until: null,
          reloadNonce: 0,
        })
      );

      // Let the initial rejection settle -> banner shows.
      await act(async () => {
        await vi.advanceTimersByTimeAsync(0);
      });
      expect(result.current.error).toBe('boom');

      // Advance past the 3s reconnect delay + the 250ms flush window.
      await act(async () => {
        await vi.advanceTimersByTimeAsync(3300);
      });

      expect(result.current.error).toBeUndefined();
      expect(result.current.logs.map((l) => l.line)).toContain('recovered');
      expect(call).toBe(2);
    } finally {
      vi.useRealTimers();
    }
  });

  it('drops the unwritten tail on a following reconnect and resumes with tail:0 + nano-since, deduping the boundary line', async () => {
    vi.useFakeTimers();
    try {
      const calls: StreamLogsParams[] = [];
      // 1st connect (following): two COMPLETE timestamped lines + a trailing
      // fragment WITHOUT a newline, then the stream ENDS normally (resolves).
      // 2nd connect (reconnect): Docker's inclusive `since` re-delivers the
      // boundary line, followed by a genuinely new line.
      // eslint-disable-next-line func-style
      const streamLogs: StreamLogsFn = (params, onChunk) => {
        calls.push(params);
        if (calls.length === 1) {
          onChunk(
            encoder.encode(
              tsLine('1', 'line-one') +
                tsLine('2', 'line-two') +
                // no trailing newline: this fragment must NOT be flushed.
                '2020-01-01T00:00:03.000000000Z partial-fragment'
            )
          );
          // following stream ended normally -> the hook must reconnect.
          return Promise.resolve();
        }
        onChunk(
          encoder.encode(
            // re-delivered boundary line (same content + timestamp): deduped.
            tsLine('2', 'line-two') +
              // new line past the resume point: kept.
              tsLine('4', 'line-three')
          )
        );
        // stay open so no further reconnect fires during the test.
        return new Promise(() => {});
      };

      const { result } = renderHook(() =>
        useLogViewer({ ...baseOptions, streamLogs, autoRefresh: true })
      );

      // Flush the 1st connect's buffered lines (250ms coalesce window).
      await act(async () => {
        await vi.advanceTimersByTimeAsync(300);
      });

      // (a) the newline-less trailing fragment is dropped, not flushed.
      expect(result.current.logs.map((l) => l.line)).toEqual([
        'line-one',
        'line-two',
      ]);

      // Advance past the 3s reconnect delay + the 250ms flush window.
      await act(async () => {
        await vi.advanceTimersByTimeAsync(3300);
      });

      // (b) the reconnect resumes: tail dropped to 0 (no historical re-backfill)
      // and `since` is the nano-precision resume point of the last COMPLETE line.
      expect(calls).toHaveLength(2);
      expect(calls[1].follow).toBe(true);
      expect(calls[1].tail).toBe(0);
      expect(calls[1].since).toBe(
        rfc3339ToUnixNanoSince('2020-01-01T00:00:02.000000000Z')
      );

      // (c) the re-delivered boundary line is deduped (appears exactly once, not
      // truncated) and the new line is appended.
      const lines = result.current.logs.map((l) => l.line);
      expect(lines).toEqual(['line-one', 'line-two', 'line-three']);
      expect(lines.filter((l) => l === 'line-two')).toHaveLength(1);
    } finally {
      vi.useRealTimers();
    }
  });

  it('clears the error banner on a silent reconnect recovery (stream opens, container idle)', async () => {
    vi.useFakeTimers();
    try {
      let call = 0;
      // 1st connect rejects (surfaces the banner); the reconnect OPENS
      // successfully but the container is idle, so no line ever arrives.
      // eslint-disable-next-line func-style
      const streamLogs: StreamLogsFn = (_params, _onChunk, _signal, onOpen) => {
        call += 1;
        if (call === 1) {
          return Promise.reject(new Error('boom'));
        }
        onOpen?.(); // stream is open, HTTP ok
        return new Promise(() => {}); // idle: stays open, emits nothing
      };

      const { result } = renderHook(() =>
        useLogViewer({ ...baseOptions, streamLogs, autoRefresh: true })
      );

      await act(async () => {
        await vi.advanceTimersByTimeAsync(0);
      });
      expect(result.current.error).toBe('boom');

      // Past the 3s reconnect delay: the stream reopens but stays silent.
      await act(async () => {
        await vi.advanceTimersByTimeAsync(3300);
      });

      // Banner cleared on open alone, even with zero new lines.
      expect(result.current.error).toBeUndefined();
      expect(result.current.logs).toHaveLength(0);
      expect(call).toBe(2);
    } finally {
      vi.useRealTimers();
    }
  });

  it('trims from the head so at most MAX_LOG_LINES are retained, newest kept', async () => {
    const TOTAL = 5001; // one over the 5000 cap
    // eslint-disable-next-line func-style
    const streamLogs: StreamLogsFn = (_params, onChunk) => {
      const body = Array.from(
        { length: TOTAL },
        (_, i) => `msg-${i}\n`
      ).join('');
      onChunk(encoder.encode(body));
      return Promise.resolve();
    };

    const { result } = renderHook(() =>
      useLogViewer({ ...baseOptions, streamLogs, autoRefresh: false })
    );

    await waitFor(() => {
      expect(result.current.logs).toHaveLength(5000);
    });
    // Oldest line trimmed, newest retained at the tail.
    const lines = result.current.logs.map((l) => l.line);
    expect(lines[lines.length - 1]).toBe(`msg-${TOTAL - 1}`);
    expect(lines).not.toContain('msg-0');
  });

  it('resets the buffer when the resource or line-count changes', async () => {
    let call = 0;
    // Each (re)connect delivers a distinct line so a stale buffer would show up.
    // eslint-disable-next-line func-style
    const streamLogs: StreamLogsFn = (_params, onChunk) => {
      call += 1;
      onChunk(encoder.encode(`batch-${call}\n`));
      return Promise.resolve();
    };

    const { result, rerender } = renderHook(
      (props: UseLogViewerOptions) => useLogViewer(props),
      { initialProps: { ...baseOptions, streamLogs, resourceId: 'c1' } }
    );

    await waitFor(() => {
      expect(result.current.logs.map((l) => l.line)).toEqual(['batch-1']);
    });

    // Changing lineCount restarts the stream from a clean buffer.
    rerender({ ...baseOptions, streamLogs, resourceId: 'c1', lineCount: 200 });
    await waitFor(() => {
      expect(result.current.logs.map((l) => l.line)).toEqual(['batch-2']);
    });

    // Changing the resource id likewise resets (no cross-resource carryover).
    rerender({ ...baseOptions, streamLogs, resourceId: 'c2', lineCount: 200 });
    await waitFor(() => {
      expect(result.current.logs.map((l) => l.line)).toEqual(['batch-3']);
    });
  });
});
