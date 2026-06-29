import moment from 'moment';

import { streamContainerLogs } from '@/react/docker/containers/containers.service';
import { createLogStreamProcessor, rfc3339ToUnixNanoSince } from '@/docker/helpers/logHelper';

// Hard cap on how many lines we keep in the DOM/buffer during a long live
// stream. We trim from the head (oldest lines) so a selection anchored in the
// tail is never disturbed. The user-facing `lineCount` (tail) is a *request*
// for how many historical lines to fetch, not this in-memory limit.
const MAX_LOG_LINES = 5000;
// Delay before reconnecting after the stream ends or errors (container stopped,
// network blip, proxy hiccup). Avoids hammering a stopped container.
const RECONNECT_DELAY_MS = 3000;
// Default tail (historical-backfill) request used on the initial connect and
// whenever the user clears/invalidates the "Lines" field — without it a cleared
// number input becomes null, `tail` is dropped, and Docker re-downloads the
// entire history of a chatty container on every parameter change.
const DEFAULT_LINE_COUNT = 100;

angular.module('portainer.docker').controller('ContainerLogsController', [
  '$scope',
  '$transition$',
  '$timeout',
  'ContainerService',
  'Notifications',
  'HttpRequestHelper',
  'endpoint',
  function ($scope, $transition$, $timeout, ContainerService, Notifications, HttpRequestHelper, endpoint) {
    $scope.state = {
      // No refreshRate here: container logs are delivered over a live stream now,
      // not a 3s poll, so there is nothing to refresh on an interval.
      lineCount: DEFAULT_LINE_COUNT,
      sinceTimestamp: '',
      displayTimestamps: false,
    };

    // Live-stream bookkeeping (not on $scope.state to avoid digest churn).
    var stream = {
      abortController: null,
      reconnectTimer: null,
      // RFC3339 timestamp of the last log line we received. Used as `since` on
      // reconnect so we resume from the exact log position (not the client
      // wall-clock) and neither duplicate nor lose lines across a reconnect.
      lastTimestamp: '',
      // Exact content of the line(s) at `lastTimestamp` we already rendered.
      // Handed to the reconnecting processor so it drops only genuine duplicates
      // Docker re-delivers, never a new line that shares the boundary nanosecond.
      boundaryLines: [],
      // false while the stream is being torn down (view destroyed / reconnect
      // teardown) — suppresses auto-reconnect.
      active: false,
      skipHeaders: false,
      // Whether we already surfaced the current reconnect-loop error, so the 3s
      // reconnect loop does not spam a notification on every attempt.
      errorNotified: false,
    };

    $scope.$on('$destroy', function () {
      stopStream();
    });

    function clearReconnectTimer() {
      if (stream.reconnectTimer) {
        $timeout.cancel(stream.reconnectTimer);
        stream.reconnectTimer = null;
      }
    }

    function abortInFlight() {
      if (stream.abortController) {
        stream.abortController.abort();
        stream.abortController = null;
      }
    }

    // Stop streaming but keep the current buffer on screen. Used for the
    // reconnect/teardown path: aborts the in-flight request and cancels any
    // pending reconnect so a fresh connect (or view destroy) starts clean.
    function pauseStream() {
      stream.active = false;
      clearReconnectTimer();
      abortInFlight();
    }

    // Full teardown on view destroy.
    function stopStream() {
      pauseStream();
    }

    function appendLines(lines) {
      if (!lines.length) {
        return;
      }
      $scope.$applyAsync(function () {
        Array.prototype.push.apply($scope.logs, lines);
        const overflow = $scope.logs.length - MAX_LOG_LINES;
        if (overflow > 0) {
          // trim oldest lines; tail (and any selection in it) is untouched
          $scope.logs.splice(0, overflow);
        }
      });
    }

    // After feeding the processor, advance the reconnect resume point to the
    // timestamp of the last line it parsed.
    function updateResumePoint(processor) {
      const ts = processor.getLastTimestamp();
      if (ts) {
        stream.lastTimestamp = ts;
        // Remember the exact boundary line(s) at this timestamp for content-exact
        // reconnect dedup (so a new line sharing the nanosecond is not dropped).
        stream.boundaryLines = processor.getBoundaryLines();
      }
    }

    // `since` is built from the last log timestamp via rfc3339ToUnixNanoSince
    // (imported from logHelper) as a single "<seconds>.<nanos>" form — see that
    // helper for why we never mix unix-seconds and RFC3339.

    // Connect (or reconnect) the live stream.
    // `resetBuffer` clears the on-screen buffer (used on first connect / param
    // changes); reconnects after a drop keep the buffer and resume via `since`.
    function startStream(resetBuffer) {
      pauseStream();
      stream.active = true;

      if (resetBuffer) {
        $scope.logs.length = 0;
        stream.lastTimestamp = '';
        stream.boundaryLines = [];
        stream.errorNotified = false;
      }

      const resuming = !!stream.lastTimestamp;

      const processor = createLogStreamProcessor({
        stripHeaders: stream.skipHeaders,
        withTimestamps: $scope.state.displayTimestamps,
        // We always request timestamps from Docker (see params below) so we can
        // resume from the exact log timestamp on reconnect; the processor parses
        // them and strips the prefix when the user has timestamps hidden.
        streamHasTimestamps: true,
        // On reconnect, drop lines Docker re-delivers at/before the resume point
        // (its `since` filter is inclusive); content-exact match on the boundary
        // line(s) so a new line sharing the boundary timestamp is not lost.
        skipUntilTimestamp: resuming ? stream.lastTimestamp : undefined,
        skipBoundaryContents: resuming ? stream.boundaryLines : undefined,
      });

      const abortController = new AbortController();
      stream.abortController = abortController;

      // On a reconnect resume from the exact timestamp of the last line we saw;
      // on the initial connect honour the user's "Fetch since" selection. Both
      // are sent as a single "<unix>.<nanos>" form (see rfc3339ToUnixNanoSince).
      let since;
      if (stream.lastTimestamp) {
        since = rfc3339ToUnixNanoSince(stream.lastTimestamp);
      } else if ($scope.state.sinceTimestamp) {
        since = `${moment($scope.state.sinceTimestamp).unix()}.000000000`;
      } else {
        since = undefined; // omitted by the service -> Docker tails from now
      }

      // Substitute the default when the "Lines" field is cleared/invalid (a
      // cleared number input is null), otherwise tail would be dropped and Docker
      // would re-download the whole history.
      const tailLineCount = $scope.state.lineCount > 0 ? $scope.state.lineCount : DEFAULT_LINE_COUNT;

      const params = {
        stdout: true,
        stderr: true,
        follow: true,
        // Always request timestamps so we can resume precisely on reconnect; the
        // processor hides them from display when the user toggle is off.
        timestamps: true,
        // tail is a historical-backfill request: apply it only on the initial
        // connect. On a reconnect we resume from `since`, so re-applying tail
        // would re-deliver the tail window. (0 is dropped by the service.)
        tail: resuming ? 0 : tailLineCount,
        since,
      };

      streamContainerLogs(
        endpoint.Id,
        $transition$.params().id,
        params,
        function onChunk(bytes) {
          const lines = processor.push(bytes);
          appendLines(lines);
          updateResumePoint(processor);
          if (lines.length) {
            // got data again -> allow a fresh error notification next failure
            stream.errorNotified = false;
          }
        },
        abortController.signal
      )
        .then(function onEnd() {
          // The stream always reconnects from here, so discard the processor's
          // unfinished trailing remainder instead of flushing it: emitting a
          // truncated fragment AND advancing the resume point to its timestamp
          // would make Docker re-send the full line under the same `since` and
          // dedup drop it — losing the line and leaving the fragment on screen.
          // Completed lines already advanced the resume point in onChunk; `since`
          // redelivers the full boundary line on reconnect.
          scheduleReconnect();
        })
        .catch(function onError(err) {
          if (abortController.signal.aborted) {
            return; // intentional abort (pause/destroy/param change)
          }
          // Drop the unfinished remainder on reconnect (see onEnd): do not flush
          // a truncated line nor move the resume point off an incomplete line.
          // Notify once per reconnect loop, not on every 3s retry.
          if (!stream.errorNotified) {
            stream.errorNotified = true;
            Notifications.error('Failure', err, 'Unable to stream container logs');
          }
          scheduleReconnect();
        });
    }

    function scheduleReconnect() {
      if (!stream.active) {
        return;
      }
      clearReconnectTimer();
      stream.reconnectTimer = $timeout(function () {
        if (stream.active) {
          startStream(false);
        }
      }, RECONNECT_DELAY_MS);
    }

    // Restart the stream from scratch when a fetch parameter changes (tail
    // count, since, timestamps). Each replaces the buffer with a fresh request.
    function restartOnParamChange() {
      if (stream.active || stream.abortController) {
        startStream(true);
      }
    }
    $scope.$watch('state.lineCount', onParamWatch);
    $scope.$watch('state.sinceTimestamp', onParamWatch);
    $scope.$watch('state.displayTimestamps', onParamWatch);
    function onParamWatch(newVal, oldVal) {
      if (newVal !== oldVal) {
        restartOnParamChange();
      }
    }

    function initView() {
      HttpRequestHelper.setPortainerAgentTargetHeader($transition$.params().nodeName);
      ContainerService.container(endpoint.Id, $transition$.params().id)
        .then(function success(data) {
          var container = data;
          $scope.container = container;

          const logsEnabled = container.HostConfig && container.HostConfig.LogConfig && container.HostConfig.LogConfig.Type && container.HostConfig.LogConfig.Type !== 'none';
          $scope.logsEnabled = logsEnabled;

          if (logsEnabled) {
            // initialise an (empty but defined) buffer so the viewer renders
            // immediately, then start the live stream
            $scope.logs = [];
            stream.skipHeaders = !container.Config.Tty;
            startStream(true);
          }
        })
        .catch(function error(err) {
          Notifications.error('Failure', err, 'Unable to retrieve container information');
        });
    }

    initView();
  },
]);
