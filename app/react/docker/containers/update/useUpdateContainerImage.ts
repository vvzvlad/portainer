import { QueryClient, useMutation, useQueryClient } from '@tanstack/react-query';

import { withError } from '@/react-tools/react-query';
import { Stack } from '@/react/common/stacks/types';
import { queryKeys as stacksQueryKeys } from '@/react/common/stacks/queries/query-keys';

import { queryKeys as containerQueryKeys } from '../queries/query-keys';

import { applyContainerUpdate } from './applyContainerUpdate';
import { ContainerUpdateContext } from './types';

/**
 * Refresh the data affected by a container image update: the container itself,
 * its image-status badge (so it flips away from "outdated") and the stacks
 * list (a stack redeploy bumps its deployment info).
 *
 * Note: for a stack redeploy this invalidates only the representative container's
 * badge, not those of its siblings in the same stack — a stack redeploy updates
 * every container, but only `context` is passed here. The sibling badges refresh
 * on their next natural refetch (staleTime / window focus) or a manual reload.
 * They are deliberately not force-invalidated from this shared helper (also used
 * by the single standalone "Update now") to avoid an endpoint-wide badge refetch.
 */
export function invalidateContainerUpdateQueries(
  queryClient: QueryClient,
  context: ContainerUpdateContext
) {
  queryClient.invalidateQueries(
    containerQueryKeys.container(context.environmentId, context.id)
  );
  queryClient.invalidateQueries(
    containerQueryKeys.imageStatus(
      context.environmentId,
      context.id,
      context.nodeName
    )
  );
  queryClient.invalidateQueries(stacksQueryKeys.base());
}

interface UpdateContainerImageParams {
  context: ContainerUpdateContext;
  stacks: Stack[];
  pullImage?: boolean;
}

/**
 * Single-container image-update mutation built on the shared apply primitive.
 * Used by the details-view "Update now" button; the bulk action drives the same
 * primitive directly (see useBulkUpdateContainerImages).
 */
export function useUpdateContainerImage() {
  const queryClient = useQueryClient();

  return useMutation({
    mutationFn: ({ context, stacks, pullImage }: UpdateContainerImageParams) =>
      applyContainerUpdate(context, stacks, { pullImage }),
    onSuccess: (_kind, { context }) => {
      invalidateContainerUpdateQueries(queryClient, context);
    },
    ...withError('Unable to update container image'),
  });
}
