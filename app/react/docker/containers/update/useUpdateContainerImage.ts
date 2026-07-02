import { QueryClient, useMutation, useQueryClient } from '@tanstack/react-query';

import { withError } from '@/react-tools/react-query';

import { queryKeys as containerQueryKeys } from '../queries/query-keys';

import { applyContainerUpdate } from './applyContainerUpdate';
import { ContainerUpdateContext } from './types';

/**
 * Refresh the data affected by a single-container image update: the container
 * itself and its image-status badge (so it flips away from "outdated"). A
 * single-container recreate doesn't change the stacks list, so that isn't
 * invalidated here.
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
}

interface UpdateContainerImageParams {
  context: ContainerUpdateContext;
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
    mutationFn: ({ context, pullImage }: UpdateContainerImageParams) =>
      applyContainerUpdate(context, { pullImage }),
    onSuccess: (_result, { context }) => {
      invalidateContainerUpdateQueries(queryClient, context);
    },
    ...withError('Unable to update container image'),
  });
}
