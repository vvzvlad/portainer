import angular from 'angular';

angular.module('portainer.kubernetes').controller('KubernetesSecurityConstraintController', [
  '$scope',
  'EndpointProvider',
  'EndpointService',
  function ($scope, EndpointProvider, EndpointService) {
    $scope.state = {
      viewReady: false,
      actionInProgress: false,
    };

    async function initView() {
      const endpointID = EndpointProvider.endpointID();
      EndpointService.endpoint(endpointID).then((endpoint) => {
        $scope.endpoint = endpoint;
        $scope.state.viewReady = true;
      });
    }

    initView();
  },
]);
