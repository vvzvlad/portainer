import { useMutation, useQueryClient } from '@tanstack/react-query';

import { Stack } from '@/react/common/stacks/types';
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
import { groupContainersForUpdate } from './groupContainersForUpdate';
import { invalidateContainerUpdateQueries } from './useUpdateContainerImage';
import { ContainerUpdateContext } from './types';

interface BulkUpdateParams {
  contexts: ContainerUpdateContext[];
  stacks: Stack[];
  /**
   * Whether the user holds `PortainerStackUpdate`. Stack redeploys are gated on
   * it everywhere else, so without it we skip (never 403) stack-managed ones.
   */
  canUpdateStack: boolean;
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
    mutationFn: ({ contexts, stacks, canUpdateStack }: BulkUpdateParams) =>
      bulkUpdate(queryClient, contexts, stacks, canUpdateStack),
  });
}

async function bulkUpdate(
  queryClient: ReturnType<typeof useQueryClient>,
  contexts: ContainerUpdateContext[],
  stacks: Stack[],
  canUpdateStack: boolean
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
    return { containersUpdated: 0, stacksUpdated: 0, failures: [], skipped: 0 };
  }

  const { standalone, stacks: stackGroups, external } =
    groupContainersForUpdate(outdated, stacks);

  // Without stack-update rights we must not attempt a stack redeploy (403).
  const allowedStackGroups = canUpdateStack ? stackGroups : [];
  const skippedUnauthorizedStacks = canUpdateStack ? 0 : stackGroups.length;

  let containersUpdated = 0;
  let stacksUpdated = 0;
  const failures: string[] = [];

  // Standalone containers: recreate-with-pull, one per container.
  await runSequential(standalone, async (context) => {
    try {
      await applyContainerUpdate(context, stacks);
      invalidateContainerUpdateQueries(queryClient, context);
      containersUpdated += 1;
    } catch (err) {
      failures.push(context.name);
      notifyError('Failure', err as Error, `Unable to update ${context.name}`);
    }
  });

  // Stack-managed containers: redeploy each owning stack exactly once.
  await runSequential(allowedStackGroups, async ({ context }) => {
    try {
      await applyContainerUpdate(context, stacks);
      invalidateContainerUpdateQueries(queryClient, context);
      stacksUpdated += 1;
    } catch (err) {
      failures.push(context.name);
      notifyError(
        'Failure',
        err as Error,
        `Unable to redeploy stack for ${context.name}`
      );
    }
  });

  // A stack redeploy updates every container in the stack, so report containers
  // and stacks separately rather than conflating both into one "update" count.
  if (containersUpdated > 0 || stacksUpdated > 0) {
    const parts: string[] = [];
    if (containersUpdated > 0) {
      parts.push(`${containersUpdated} ${pluralize(containersUpdated, 'container')}`);
    }
    if (stacksUpdated > 0) {
      parts.push(`${stacksUpdated} ${pluralize(stacksUpdated, 'stack')}`);
    }
    notifySuccess('Success', `${parts.join(' and ')} updated`);
  }

  const skippedExternal = external.length;
  if (
    skippedNotOutdated > 0 ||
    skippedExternal > 0 ||
    skippedUnauthorizedStacks > 0
  ) {
    const parts: string[] = [];
    if (skippedNotOutdated > 0) {
      parts.push(`${skippedNotOutdated} not outdated`);
    }
    if (skippedExternal > 0) {
      parts.push(`${skippedExternal} managed outside Portainer`);
    }
    if (skippedUnauthorizedStacks > 0) {
      parts.push(
        `${skippedUnauthorizedStacks} ${pluralize(
          skippedUnauthorizedStacks,
          'stack'
        )} you can't update`
      );
    }
    notifyWarning('Some containers were skipped', parts.join(', '));
  }

  return {
    containersUpdated,
    stacksUpdated,
    failures,
    skipped: skippedNotOutdated + skippedExternal + skippedUnauthorizedStacks,
  };
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
