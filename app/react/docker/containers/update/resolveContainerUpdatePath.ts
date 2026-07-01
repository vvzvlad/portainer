import { COMPOSE_STACK_NAME_LABEL } from '@/react/constants';
import { Stack, StackType } from '@/react/common/stacks/types';
import { EnvironmentId } from '@/react/portainer/environments/types';

import { ContainerUpdatePath } from './types';

/**
 * Decide how a container's image update must be applied, given the list of
 * Portainer-managed stacks. Pure and side-effect free so it can be unit-tested
 * and reused by the details button and the bulk action. (The backend auto-update
 * daemon mirrors the same routing separately in Go.)
 *
 * - No compose project label -> `standalone` (recreate-with-pull).
 * - Compose project that matches a Portainer Docker Compose `Stack` (same name +
 *   endpoint + compose type) -> `stack` (redeploy-with-pull, keeps the container
 *   in its stack).
 * - Compose project with no matching Portainer compose stack -> `external`:
 *   managed outside Portainer (or a same-named stack of another type), so we
 *   must not recreate it (would detach it / drift).
 */
export function resolveContainerUpdatePath(
  context: { labels?: Record<string, string>; environmentId: EnvironmentId },
  stacks: Stack[]
): ContainerUpdatePath {
  const projectName = context.labels?.[COMPOSE_STACK_NAME_LABEL];

  if (!projectName) {
    return { kind: 'standalone' };
  }

  // Match by name + endpoint, but only a Docker Compose stack: a same-named
  // swarm/kubernetes stack must not be redeployed via the compose update path.
  const stack = stacks.find(
    (s) =>
      s.Name === projectName &&
      s.EndpointId === context.environmentId &&
      s.Type === StackType.DockerCompose
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
