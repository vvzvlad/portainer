import clsx from 'clsx';
import { BarChart, FileText, Info, Paperclip, Terminal } from 'lucide-react';

import { ContainerStatus } from '@/react/docker/containers/types';
import {
  STACK_CONTAINER_STATE_NAME,
  StackContainerLinkParams,
} from '@/react/docker/containers/ItemView/containerBreadcrumbs';
import { Authorized } from '@/react/hooks/useUser';

import { Icon } from '@@/Icon';
import { Link } from '@@/Link';

import styles from './ContainerQuickActions.module.css';

export interface QuickActionsState {
  showQuickActionAttach: boolean;
  showQuickActionExec: boolean;
  showQuickActionInspect: boolean;
  showQuickActionLogs: boolean;
  showQuickActionStats: boolean;
}

export function ContainerQuickActions({
  status,
  containerId,
  nodeName,
  state,
  stackLinkParams,
}: {
  containerId: string;
  nodeName: string;
  status: ContainerStatus;
  state: QuickActionsState;
  // When provided (container opened from a stack), the quick actions link to
  // the stack-scoped container sub-tab states with these params so the stack
  // breadcrumb trail is preserved. Otherwise they link to the global states.
  stackLinkParams?: StackContainerLinkParams;
}) {
  const isActive =
    !!status &&
    [
      ContainerStatus.Starting,
      ContainerStatus.Running,
      ContainerStatus.Healthy,
      ContainerStatus.Unhealthy,
    ].includes(status);

  // Build the target ui-router state for a given sub-tab: the stack-scoped
  // state when inside a stack, the global container state otherwise. Both share
  // the same tab suffix (logs/inspect/stats/exec/attach).
  function linkTo(tab: string) {
    return stackLinkParams
      ? `${STACK_CONTAINER_STATE_NAME}.${tab}`
      : `docker.containers.container.${tab}`;
  }
  const linkParams = stackLinkParams ?? { id: containerId, nodeName };

  return (
    <div className={clsx('space-x-1', styles.root)}>
      {state.showQuickActionLogs && (
        <Authorized authorizations="DockerContainerLogs">
          <Link
            to={linkTo('logs')}
            params={linkParams}
            title="Logs"
            data-cy={`container-logs-${containerId}`}
          >
            <Icon icon={FileText} className="space-right" />
          </Link>
        </Authorized>
      )}

      {state.showQuickActionInspect && (
        <Authorized authorizations="DockerContainerInspect">
          <Link
            to={linkTo('inspect')}
            params={linkParams}
            title="Inspect"
            data-cy={`container-inspect-${containerId}`}
          >
            <Icon icon={Info} className="space-right" />
          </Link>
        </Authorized>
      )}

      {state.showQuickActionStats && isActive && (
        <Authorized authorizations="DockerContainerStats">
          <Link
            to={linkTo('stats')}
            params={linkParams}
            title="Stats"
            data-cy={`container-stats-${containerId}`}
          >
            <Icon icon={BarChart} className="space-right" />
          </Link>
        </Authorized>
      )}

      {state.showQuickActionExec && isActive && (
        <Authorized authorizations="DockerExecStart">
          <Link
            to={linkTo('exec')}
            params={linkParams}
            title="Exec Console"
            data-cy={`container-exec-${containerId}`}
          >
            <Icon icon={Terminal} className="space-right" />
          </Link>
        </Authorized>
      )}

      {state.showQuickActionAttach && isActive && (
        <Authorized authorizations="DockerContainerAttach">
          <Link
            to={linkTo('attach')}
            params={linkParams}
            title="Attach Console"
            data-cy={`container-attach-${containerId}`}
          >
            <Icon icon={Paperclip} className="space-right" />
          </Link>
        </Authorized>
      )}
    </div>
  );
}
