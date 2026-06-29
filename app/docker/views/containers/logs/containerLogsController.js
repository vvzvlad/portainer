import moment from 'moment';

import { streamContainerLogs } from '@/react/docker/containers/containers.service';
import { createLogStreamProcessor } from '@/docker/helpers/logHelper';

// Hard cap on how many lines we keep in the DOM/buffer during a long live
// stream. We trim from the head (oldest lines) so a selection anchored in the
// tail is never disturbed. The user-facing `lineCount` (tail) is a *request*
// for how many historical lines to fetch, not this in-memory limit.
const MAX_LOG_LINES = 5000;
// Delay before reconnecting after the stream ends or errors (container stopped,
// network blip, proxy hiccup). Avoids hammering a stopped container.
const RECONNECT_DELAY_MS = 3000;

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
      // refreshRate kept for backwards compat with any external references; the
      // transport is now a live stream, not a poll, so it is unused.
      lineCount: 100,
      sinceTimestamp: '',
      displayTimestamps: false,
    };

    // Live-stream bookkeeping (not on $scope.state to avoid digest churn).
    var stream = {
      abortController: null,
      reconnectTimer: null,
      // wall-clock seconds of the last received chunk; used as `since` on
      // reconnect so we neither duplicate nor lose lines across a reconnect.
      lastReceivedAt: 0,
      // false while the stream is intentionally paused (Live toggle off) or the
      // view is being destroyed — suppresses auto-reconnect.
      active: false,
      skipHeaders: false,
    };

    // Live toggle (the "Auto-refresh logs"/Live switch in the viewer).
    $scope.changeLogCollection = function (logCollectionStatus) {
      if (!logCollectionStatus) {
        pauseStream();
      } else {
        startStream(true);
      }
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

    // Pause: stop streaming but keep the current buffer on screen.
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
      stream.lastReceivedAt = moment().unix();
      $scope.$applyAsync(function () {
        Array.prototype.push.apply($scope.logs, lines);
        const overflow = $scope.logs.length - MAX_LOG_LINES;
        if (overflow > 0) {
          // trim oldest lines; tail (and any selection in it) is untouched
          $scope.logs.splice(0, overflow);
        }
      });
    }

    // Connect (or reconnect) the live stream.
    // `resetBuffer` clears the on-screen buffer (used on first connect / param
    // changes); reconnects after a drop keep the buffer and resume via `since`.
    function startStream(resetBuffer) {
      pauseStream();
      stream.active = true;

      if (resetBuffer) {
        $scope.logs.length = 0;
        stream.lastReceivedAt = 0;
      }

      const processor = createLogStreamProcessor({
        stripHeaders: stream.skipHeaders,
        withTimestamps: $scope.state.displayTimestamps,
      });

      const abortController = new AbortController();
      stream.abortController = abortController;

      // On a reconnect resume from the last line we saw (seconds granularity);
      // on the initial connect honour the user's "Fetch since" selection.
      const sinceFromUser = $scope.state.sinceTimestamp ? moment($scope.state.sinceTimestamp).unix() : 0;
      const since = stream.lastReceivedAt || sinceFromUser;

      const params = {
        stdout: true,
        stderr: true,
        follow: true,
        timestamps: $scope.state.displayTimestamps,
        // tail is a historical-backfill request: apply it only on the initial
        // connect. On a reconnect we resume from `since`, so re-applying tail
        // would re-deliver the tail window. (0 is dropped by the service.)
        tail: stream.lastReceivedAt ? 0 : $scope.state.lineCount,
        since,
      };

      streamContainerLogs(
        endpoint.Id,
        $transition$.params().id,
        params,
        function onChunk(text) {
          appendLines(processor.push(text));
        },
        abortController.signal
      )
        .then(function onEnd() {
          appendLines(processor.flush());
          scheduleReconnect();
        })
        .catch(function onError(err) {
          if (abortController.signal.aborted) {
            return; // intentional abort (pause/destroy/param change)
          }
          scheduleReconnect();
          Notifications.error('Failure', err, 'Unable to stream container logs');
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
