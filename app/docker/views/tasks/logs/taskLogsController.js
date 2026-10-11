import moment from 'moment';

angular.module('portainer.docker').controller('TaskLogsController', [
  '$scope',
  '$transition$',
  '$interval',
  'TaskService',
  'ServiceService',
  'Notifications',
  function ($scope, $transition$, $interval, TaskService, ServiceService, Notifications) {
    $scope.state = {
      refreshRate: 3,
      lineCount: 100,
      sinceTimestamp: '',
      displayTimestamps: false,
    };

    $scope.$on('$destroy', function () {
      stopRepeater();
    });

    function stopRepeater() {
      var repeater = $scope.repeater;
      if (angular.isDefined(repeater)) {
        $interval.cancel(repeater);
      }
    }

    function setUpdateRepeater() {
      var refreshRate = $scope.state.refreshRate;
      $scope.repeater = $interval(function () {
        TaskService.logs($transition$.params().id, 1, 1, $scope.state.displayTimestamps ? 1 : 0, moment($scope.state.sinceTimestamp).unix(), $scope.state.lineCount)
          .then(function success(data) {
            // Task logs poll and replace the whole array, and formatLogs
            // assigns fresh line ids per poll. Assign positionally-stable ids
            // (0..N) so `track by log.id` reuses rows across polls and a live
            // text selection survives.
            $scope.logs = data.map(function (line, i) {
              return { ...line, id: i };
            });
          })
          .catch(function error(err) {
            stopRepeater();
            Notifications.error('Failure', err, 'Unable to retrieve task logs');
          });
      }, refreshRate * 1000);
    }

    function startLogPolling() {
      TaskService.logs($transition$.params().id, 1, 1, $scope.state.displayTimestamps ? 1 : 0, moment($scope.state.sinceTimestamp).unix(), $scope.state.lineCount)
        .then(function success(data) {
          // Positionally-stable ids so `track by log.id` reuses rows (see the
          // poll handler above).
          $scope.logs = data.map(function (line, i) {
            return { ...line, id: i };
          });
          setUpdateRepeater();
        })
        .catch(function error(err) {
          stopRepeater();
          Notifications.error('Failure', err, 'Unable to retrieve task logs');
        });
    }

    function initView() {
      TaskService.task($transition$.params().id)
        .then(function success(data) {
          var task = data;
          $scope.task = task;
          return ServiceService.service(task.ServiceId);
        })
        .then(function success(data) {
          var service = data;
          $scope.service = service;
          startLogPolling();
        })
        .catch(function error(err) {
          Notifications.error('Failure', err, 'Unable to retrieve task details');
        });
    }

    initView();
  },
]);
