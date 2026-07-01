import { CellContext } from '@tanstack/react-table';

import { useAuthorizations } from '@/react/hooks/useUser';
import { ContainerQuickActions } from '@/react/docker/containers/components/ContainerQuickActions';
import { ContainerListViewModel } from '@/react/docker/containers/types';
import { buildStackContainerLinkParams } from '@/react/docker/containers/ItemView/containerBreadcrumbs';

import { useTableSettings } from '@@/datatables/useTableSettings';

import { TableSettings } from '../types';
import { useRowContext } from '../RowContext';

import { columnHelper } from './helper';

export const quickActions = columnHelper.display({
  header: 'Quick Actions',
  id: 'actions',
  cell: QuickActionsCell,
});

function QuickActionsCell({
  row: { original: container },
}: CellContext<ContainerListViewModel, unknown>) {
  const settings = useTableSettings<TableSettings>();
  const { stackRouteParams } = useRowContext();

  const { hiddenQuickActions = [] } = settings;

  // When rendered inside a stack, build the params for the stack-scoped
  // container sub-tab states so the stack breadcrumb trail is preserved.
  // Undefined in the global containers list, which keeps the global links.
  const containerStackLinkParams = stackRouteParams
    ? buildStackContainerLinkParams(
        { ...stackRouteParams, nodeName: container.NodeName },
        container.Id
      )
    : undefined;

  const wrapperState = {
    showQuickActionAttach: !hiddenQuickActions.includes('attach'),
    showQuickActionExec: !hiddenQuickActions.includes('exec'),
    showQuickActionInspect: !hiddenQuickActions.includes('inspect'),
    showQuickActionLogs: !hiddenQuickActions.includes('logs'),
    showQuickActionStats: !hiddenQuickActions.includes('stats'),
  };

  const someOn =
    wrapperState.showQuickActionAttach ||
    wrapperState.showQuickActionExec ||
    wrapperState.showQuickActionInspect ||
    wrapperState.showQuickActionLogs ||
    wrapperState.showQuickActionStats;

  const { authorized } = useAuthorizations([
    'DockerContainerStats',
    'DockerContainerLogs',
    'DockerExecStart',
    'DockerContainerInspect',
    'DockerTaskInspect',
    'DockerTaskLogs',
  ]);

  if (!someOn || !authorized) {
    return null;
  }

  return (
    <ContainerQuickActions
      containerId={container.Id}
      nodeName={container.NodeName}
      status={container.Status}
      state={wrapperState}
      stackLinkParams={containerStackLinkParams}
    />
  );
}
