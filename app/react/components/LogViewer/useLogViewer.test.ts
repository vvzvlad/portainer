import { waitFor } from '@testing-library/react';
import { act, renderHook } from '@testing-library/react-hooks';

import { StreamLogsParams } from '@/react/docker/containers/containers.service';

import { StreamLogsFn } from './types';
import { useLogViewer } from './useLogViewer';

const encoder = new TextEncoder();

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
});
