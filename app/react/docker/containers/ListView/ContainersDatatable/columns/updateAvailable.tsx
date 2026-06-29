import { CellContext } from '@tanstack/react-table';

import type { ContainerListViewModel } from '@/react/docker/containers/types';
import { useEnvironmentId } from '@/react/hooks/useEnvironmentId';
import { useContainerImageStatus } from '@/react/docker/containers/queries/useContainerImageStatus';

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

  return (
    <UpdateStatusBadge
      status={statusQuery.data?.Status}
      isLoading={statusQuery.isLoading}
    />
  );
}
