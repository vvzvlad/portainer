import { trimContainerName } from '@/docker/filters/utils';
import { getContainerSubTabBreadcrumbs } from '@/react/docker/containers/ItemView/containerBreadcrumbs';

// The container logs page is now rendered entirely by the React
// <container-log-view> (see containerlogs.html), which owns fetching, live
// tailing, formatting and colouring. This controller only provides the page
// breadcrumbs and sets the agent-target header for the node the container lives
// on, so the React log stream resolves the correct node on Agent/Edge
// environments.
angular.module('portainer.docker').controller('ContainerLogsController', [
  '$scope',
  '$transition$',
  'ContainerService',
  'Notifications',
  'HttpRequestHelper',
  'endpoint',
  function ($scope, $transition$, ContainerService, Notifications, HttpRequestHelper, endpoint) {
    function initView() {
      HttpRequestHelper.setPortainerAgentTargetHeader($transition$.params().nodeName);
      // Set the trail up-front (without the container name) so it survives the
      // load window and a load error; the success path fills in the name.
      $scope.breadcrumbs = getContainerSubTabBreadcrumbs($transition$.to().name, $transition$.params(), '', 'Logs');
      ContainerService.container(endpoint.Id, $transition$.params().id)
        .then(function success(container) {
          // Stack-aware breadcrumb: keeps the stack trail when the container was
          // opened from a stack, falls back to the global Containers trail otherwise.
          $scope.breadcrumbs = getContainerSubTabBreadcrumbs($transition$.to().name, $transition$.params(), trimContainerName(container.Name), 'Logs');
        })
        .catch(function error(err) {
          Notifications.error('Failure', err, 'Unable to retrieve container information');
        });
    }

    initView();
  },
]);
