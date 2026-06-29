import angular from 'angular';

import { NotificationsViewAngular } from '@/react/portainer/notifications/NotificationsView';
import { AccessHeaders } from '../authorization-guard';
import { UserActivityService } from './user-activity.service';
import { UserActivity } from './user-activity.rest';

export default angular
  .module('portainer.app.user-activity', [])
  .service('UserActivity', UserActivity)
  .service('UserActivityService', UserActivityService)
  .component('notifications', NotificationsViewAngular)
  .config(config).name;

/* @ngInject */
function config($stateRegistryProvider) {
  $stateRegistryProvider.register({
    name: 'portainer.activityLogs',
    url: '/activity-logs',
    views: {
      'content@': {
        component: 'activityLogsView',
      },
    },
    data: {
      docs: '/admin/logs/activity',
      access: AccessHeaders.Admin,
    },
  });

  $stateRegistryProvider.register({
    name: 'portainer.notifications',
    url: '/notifications',
    views: {
      'content@': {
        component: 'notifications',
      },
    },
    params: {
      id: '',
    },
    data: {
      docs: '/admin/notifications',
    },
  });
}
