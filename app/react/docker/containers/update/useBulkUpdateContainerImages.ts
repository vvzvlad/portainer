import { useMutation, useQueryClient } from '@tanstack/react-query';

import {
  notifyError,
  notifySuccess,
  notifyWarning,
} from '@/portainer/services/notifications';

import {
  getContainerImageStatus,
  STALE_TIME,
} from '../queries/useContainerImageStatus';
import { queryKeys as containerQueryKeys } from '../queries/query-keys';

import { applyContainerUpdate } from './applyContainerUpdate';
import { invalidateContainerUpdateQueries } from './useUpdateContainerImage';
import { ContainerUpdateContext } from './types';

interface BulkUpdateParams {
  contexts: ContainerUpdateContext[];
}

/**
 * Bulk "Update selected": recreates each selected container that is `outdated`
 * with a fresh image pull (Watchtower-style, one recreate per container), and
 * skips up-to-date/unknown ones with a summary. Reports per-item success/failure.
 */
export function useBulkUpdateContainerImages() {
  const queryClient = useQueryClient();

  return useMutation({
    mutationFn: ({ contexts }: BulkUpdateParams) =>
      bulkUpdate(queryClient, contexts),
  });
}

async function bulkUpdate(
  queryClient: ReturnType<typeof useQueryClient>,
  contexts: ContainerUpdateContext[]
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
          { staleTime: STALE_TIME }
        )
        .then((status) => status.Status)
        .catch(() => 'error' as const)
    )
  );

  const outdated = contexts.filter((_c, i) => statuses[i] === 'outdated');
  const skippedNotOutdated = contexts.length - outdated.length;

  // Nothing actionable: make the no-op explicit instead of a vague "skipped".
  if (outdated.length === 0) {
    notifyWarning(
      'Nothing to update',
      'None of the selected containers have updates available.'
    );
    return { containersUpdated: 0, failures: [], skipped: 0 };
  }

  let containersUpdated = 0;
  const failures: string[] = [];

  // Recreate each outdated container individually (Watchtower-style).
  await runSequential(outdated, async (context) => {
    try {
      await applyContainerUpdate(context);
      invalidateContainerUpdateQueries(queryClient, context);
      containersUpdated += 1;
    } catch (err) {
      failures.push(context.name);
      notifyError('Failure', err as Error, `Unable to update ${context.name}`);
    }
  });

  if (containersUpdated > 0) {
    notifySuccess(
      'Success',
      `${containersUpdated} ${pluralize(
        containersUpdated,
        'container'
      )} updated`
    );
  }

  if (skippedNotOutdated > 0) {
    notifyWarning(
      'Some containers were skipped',
      `${skippedNotOutdated} not outdated`
    );
  }

  return {
    containersUpdated,
    failures,
    skipped: skippedNotOutdated,
  };
}

async function runSequential<T>(
  items: T[],
  fn: (item: T) => Promise<void>
): Promise<void> {
  // Sequential to avoid hammering registries with parallel pulls.
  await items.reduce(
    (chain, item) => chain.then(() => fn(item)),
    Promise.resolve()
  );
}

function pluralize(count: number, word: string) {
  return count === 1 ? word : `${word}s`;
}
