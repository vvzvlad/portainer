import { useMutation, useQueryClient } from '@tanstack/react-query';

import { Stack } from '@/react/common/stacks/types';
import {
  notifyError,
  notifySuccess,
  notifyWarning,
} from '@/portainer/services/notifications';

import { getContainerImageStatus } from '../queries/useContainerImageStatus';
import { queryKeys as containerQueryKeys } from '../queries/query-keys';

import { applyContainerUpdate } from './applyContainerUpdate';
import { groupContainersForUpdate } from './groupContainersForUpdate';
import { invalidateContainerUpdateQueries } from './useUpdateContainerImage';
import { ContainerUpdateContext } from './types';

// Mirror useContainerImageStatus's client-side staleTime so the bulk action
// reuses cached badge statuses instead of re-hitting the registry per row.
const STATUS_STALE_TIME = 5 * 60 * 1000;

interface BulkUpdateParams {
  contexts: ContainerUpdateContext[];
  stacks: Stack[];
}

/**
 * Bulk "Update selected": applies the shared update primitive to each selected
 * container that is `outdated`, skipping up-to-date/unknown ones with a summary,
 * and grouping stack containers so each owning stack redeploys ONCE even if
 * several of its containers were selected. Reports per-item success/failure.
 */
export function useBulkUpdateContainerImages() {
  const queryClient = useQueryClient();

  return useMutation({
    mutationFn: ({ contexts, stacks }: BulkUpdateParams) =>
      bulkUpdate(queryClient, contexts, stacks),
  });
}

async function bulkUpdate(
  queryClient: ReturnType<typeof useQueryClient>,
  contexts: ContainerUpdateContext[],
  stacks: Stack[]
) {
  // Resolve each container's status (cached where the badge already loaded it).
  const statuses = await Promise.all(
    contexts.map((context) =>
      queryClient
        .fetchQuery(
          containerQueryKeys.imageStatus(
            context.environmentId,
            context.id,
            context.nodeName
          ),
          () =>
            getContainerImageStatus(
              context.environmentId,
              context.id,
              context.nodeName
            ),
          { staleTime: STATUS_STALE_TIME }
        )
        .then((status) => status.Status)
        .catch(() => 'error' as const)
    )
  );

  const outdated = contexts.filter((_c, i) => statuses[i] === 'outdated');
  const skippedNotOutdated = contexts.length - outdated.length;

  const { standalone, stacks: stackGroups, external } =
    groupContainersForUpdate(outdated, stacks);

  let succeeded = 0;
  const failures: string[] = [];

  // Standalone containers: recreate-with-pull, one per container.
  await runSequential(standalone, async (context) => {
    try {
      await applyContainerUpdate(context, stacks);
      invalidateContainerUpdateQueries(queryClient, context);
      succeeded += 1;
    } catch (err) {
      failures.push(context.name);
      notifyError('Failure', err as Error, `Unable to update ${context.name}`);
    }
  });

  // Stack-managed containers: redeploy each owning stack exactly once.
  await runSequential(stackGroups, async ({ context }) => {
    try {
      await applyContainerUpdate(context, stacks);
      invalidateContainerUpdateQueries(queryClient, context);
      succeeded += 1;
    } catch (err) {
      failures.push(context.name);
      notifyError(
        'Failure',
        err as Error,
        `Unable to redeploy stack for ${context.name}`
      );
    }
  });

  if (succeeded > 0) {
    notifySuccess(
      'Success',
      `${succeeded} ${pluralize(succeeded, 'update')} applied`
    );
  }

  const skippedExternal = external.length;
  if (skippedNotOutdated > 0 || skippedExternal > 0) {
    const parts: string[] = [];
    if (skippedNotOutdated > 0) {
      parts.push(`${skippedNotOutdated} not outdated`);
    }
    if (skippedExternal > 0) {
      parts.push(`${skippedExternal} managed outside Portainer`);
    }
    notifyWarning('Some containers were skipped', parts.join(', '));
  }

  return { succeeded, failures, skipped: skippedNotOutdated + skippedExternal };
}

async function runSequential<T>(
  items: T[],
  fn: (item: T) => Promise<void>
): Promise<void> {
  // Sequential to avoid hammering registries / overlapping stack redeploys.
  await items.reduce(
    (chain, item) => chain.then(() => fn(item)),
    Promise.resolve()
  );
}

function pluralize(count: number, word: string) {
  return count === 1 ? word : `${word}s`;
}
