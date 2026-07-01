import { CellContext } from '@tanstack/react-table';

import type { ContainerListViewModel } from '@/react/docker/containers/types';
import { useEnvironmentId } from '@/react/hooks/useEnvironmentId';
import {
  useContainerImageStatus,
  useRecheckContainerImageStatus,
} from '@/react/docker/containers/queries/useContainerImageStatus';
import { useApplyContainerImageUpdate } from '@/react/docker/containers/update';

import { TooltipWithChildren } from '@@/Tip/TooltipWithChildren';

import { UpdateStatusBadge } from '../../../components/UpdateStatusBadge';

import { columnHelper } from './helper';

export const updateAvailable = columnHelper.display({
  header: 'Update available',
  id: 'updateAvailable',
  cell: UpdateAvailableCell,
});

function UpdateAvailableCell({
  row: { original: container },
}: CellContext<ContainerListViewModel, unknown>) {
  const environmentId = useEnvironmentId();
  // One detection request per visible row is acceptable: the backend caches results
  // for ~5m and the hook keeps a generous client-side staleTime, so re-renders and
  // pagination don't re-hit the registry. The query is non-blocking, so the table
  // renders immediately and badges fill in as statuses resolve.
  const statusQuery = useContainerImageStatus(
    environmentId,
    container.Id,
    container.NodeName
  );

  // Manual re-check forces a cache-bypassing registry comparison and writes the
  // fresh result back into the status query cache, so the badge flips if it
  // changed. (A plain refetch would only bypass the client staleTime and still
  // hit the ~5m server cache.)
  const recheckMutation = useRecheckContainerImageStatus(
    environmentId,
    container.Id,
    container.NodeName
  );

  // Same shared apply flow as the details-view "Update now" button (confirm +
  // standalone/stack/external routing + permission gating). No `onSuccess`: the
  // mutation's query invalidation refreshes this row's badge in place.
  const {
    apply,
    isLoading: isUpdating,
    isExternal,
    stackUpdateForbidden,
    canApply,
  } = useApplyContainerImageUpdate({
    environmentId,
    containerId: container.Id,
    nodeName: container.NodeName,
    containerImage: container.Image,
    containerName: container.Names?.[0] ?? container.Id,
    labels: container.Labels,
    isPortainer: container.IsPortainer ?? false,
  });

  const status = statusQuery.data?.Status;

  const badge = (
    <UpdateStatusBadge
      status={status}
      isLoading={statusQuery.isLoading}
      // Externally-managed / permission-gated updates stay non-actionable: no
      // click handler, so the badge renders as a plain span (wrapped below in a
      // tooltip that explains why).
      onUpdateClick={
        status === 'outdated' && canApply ? () => apply() : undefined
      }
      isUpdating={isUpdating}
      onRecheckClick={
        status === 'updated' ? () => recheckMutation.mutate() : undefined
      }
      isRechecking={recheckMutation.isLoading}
    />
  );

  // Mirror the details-view button's explanations for the two gated cases so a
  // user understands why the "Update available" badge isn't clickable here.
  if (status === 'outdated' && isExternal) {
    return (
      <TooltipWithChildren message="This container belongs to a compose project that is managed outside Portainer, so it can't be updated from here.">
        <span>{badge}</span>
      </TooltipWithChildren>
    );
  }

  if (status === 'outdated' && stackUpdateForbidden) {
    return (
      <TooltipWithChildren message="Updating this container redeploys its stack, which requires stack update permission you don't have.">
        <span>{badge}</span>
      </TooltipWithChildren>
    );
  }

  return badge;
}
