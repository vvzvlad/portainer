import angular from 'angular';

import { templatesModule } from './templates';
import { jobsModule } from './jobs';
import { stacksModule } from './edge-stacks';
import { groupsModule } from './groups';

export const viewsModule = angular.module('portainer.edge.react.views', [
  templatesModule,
  jobsModule,
  stacksModule,
  groupsModule,
]).name;
