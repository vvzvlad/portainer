import { Stack } from '@/react/common/stacks/types';
import { getStackFile } from '@/react/common/stacks/queries/useStackFile';
import { updateStack } from '@/react/docker/stacks/useUpdateStack';
import { updateGitStack } from '@/react/portainer/gitops/queries/useUpdateGitStack';

import { recreateContainer } from '../containers.service';

import { resolveContainerUpdatePath } from './resolveContainerUpdatePath';
import { ContainerUpdateContext, ContainerUpdateKind } from './types';

/** Thrown when an update is attempted on an externally-managed compose container. */
export const EXTERNAL_STACK_UPDATE_ERROR =
  'This container is part of a compose project that is not managed by Portainer, so it cannot be updated from here.';

/**
 * Redeploy a Portainer stack, forcing a re-pull, while preserving its current
 * file content, environment variables and options. Reuses the exact mutations
 * the stack page uses for "pull and redeploy" so we never invent a new path.
 */
async function redeployStackWithPull(stack: Stack, pullImage: boolean) {
  if (stack.GitConfig) {
    await updateGitStack(stack.Id, stack.EndpointId, {
      RepullImageAndRedeploy: pullImage,
      Env: stack.Env ?? [],
      Prune: stack.Option?.Prune,
    });
    return;
  }

  // File/standalone compose stack: redeploy with its current file unchanged.
  const file = await getStackFile({ stackId: stack.Id });
  await updateStack({
    stackId: stack.Id,
    environmentId: stack.EndpointId,
    payload: {
      stackFileContent: file.StackFileContent,
      env: stack.Env ?? [],
      prune: stack.Option?.Prune,
      repullImageAndRedeploy: pullImage,
    },
  });
}

/**
 * Shared "apply an image update" primitive. Decides standalone-vs-stack-vs-external
 * and runs the matching mutation. This is the single code path behind the
 * "Update now" button, the bulk "Update selected" action and (future M4) the
 * auto-update job, guaranteeing manual and automatic updates behave identically.
 *
 * A stack-managed container is ALWAYS routed through stack redeploy so it stays
 * part of its stack; an externally-managed compose container is refused rather
 * than recreated out-of-band (which would detach it).
 */
export async function applyContainerUpdate(
  context: ContainerUpdateContext,
  stacks: Stack[],
  { pullImage = true }: { pullImage?: boolean } = {}
): Promise<ContainerUpdateKind> {
  const path = resolveContainerUpdatePath(context, stacks);

  switch (path.kind) {
    case 'standalone':
      await recreateContainer(context.environmentId, context.id, pullImage, {
        nodeName: context.nodeName,
      });
      return 'standalone';

    case 'stack': {
      const stack = stacks.find((s) => s.Id === path.stackId);
      if (!stack) {
        // Should not happen (resolve found it), but never fall back to a bare
        // recreate: that would orphan the container from its stack.
        throw new Error(EXTERNAL_STACK_UPDATE_ERROR);
      }
      await redeployStackWithPull(stack, pullImage);
      return 'stack';
    }

    case 'external':
    default:
      throw new Error(EXTERNAL_STACK_UPDATE_ERROR);
  }
}
