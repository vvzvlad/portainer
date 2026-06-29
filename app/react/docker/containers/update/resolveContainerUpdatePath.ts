import { COMPOSE_STACK_NAME_LABEL } from '@/react/constants';
import { Stack } from '@/react/common/stacks/types';
import { EnvironmentId } from '@/react/portainer/environments/types';

import { ContainerUpdatePath } from './types';

/**
 * Decide how a container's image update must be applied, given the list of
 * Portainer-managed stacks. Pure and side-effect free so it can be unit-tested
 * and reused by the details button, the bulk action and the M4 auto-update job.
 *
 * - No compose project label -> `standalone` (recreate-with-pull).
 * - Compose project that matches a Portainer `Stack` (same name + endpoint) ->
 *   `stack` (redeploy-with-pull, keeps the container in its stack).
 * - Compose project with no matching Portainer stack -> `external`: managed
 *   outside Portainer, so we must not recreate it (would detach it / drift).
 */
export function resolveContainerUpdatePath(
  context: { labels?: Record<string, string>; environmentId: EnvironmentId },
  stacks: Stack[]
): ContainerUpdatePath {
  const projectName = context.labels?.[COMPOSE_STACK_NAME_LABEL];

  if (!projectName) {
    return { kind: 'standalone' };
  }

  const stack = stacks.find(
    (s) => s.Name === projectName && s.EndpointId === context.environmentId
  );

  if (!stack) {
    return { kind: 'external' };
  }

  return {
    kind: 'stack',
    stackId: stack.Id,
    isGitStack: !!stack.GitConfig,
  };
}
