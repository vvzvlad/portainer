import { ResourceControlViewModel } from '@/react/portainer/access-control/models/ResourceControlViewModel';

import { DockerContainerResponse } from './types/response';

export enum ContainerStatus {
  Paused = 'paused',
  Stopped = 'stopped',
  Created = 'created',
  Healthy = 'healthy',
  Unhealthy = 'unhealthy',
  Starting = 'starting',
  Running = 'running',
  Dead = 'dead',
  Exited = 'exited',
  Restarting = 'restarting',
  Removing = 'removing',
}

export type QuickAction = 'attach' | 'exec' | 'inspect' | 'logs' | 'stats';

export interface Port {
  host?: string;
  public: number;
  private: number;
}

export type ContainerId = string;

/**
 * Computed fields from Container List Raw data
 */
type DecoratedDockerContainer = {
  NodeName: string;
  ResourceControl?: ResourceControlViewModel;
  IP: string;
  StackName?: string;
  Status: ContainerStatus;
  Ports: Port[];
  StatusText: string;
  Gpus: string;
};

/**
 * Docker Container list ViewModel
 *
 * Alias AngularJS ContainerViewModel
 *
 * Raw details is ContainerDetailsJSON
 */
export type ContainerListViewModel = DecoratedDockerContainer &
  Omit<DockerContainerResponse, keyof DecoratedDockerContainer>;

export type ContainerLogsParams = {
  stdout?: boolean;
  stderr?: boolean;
  timestamps?: boolean;
  // Unix timestamp. The live-stream path sends a "<seconds>.<nanos>" string so a
  // reconnect can resume at exact nanosecond precision (which a number cannot
  // hold); the buffered axios path passes a plain number.
  since?: number | string;
  // Unix timestamp upper bound. Only relevant for a bounded snapshot fetch (Auto
  // refresh off with a from–to range selected); the live-tail path never sets it.
  until?: number | string;
  tail?: number;
};
