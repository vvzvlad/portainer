import { StackId } from '@/react/common/stacks/types';
import { EnvironmentId } from '@/react/portainer/environments/types';

import { ContainerId } from '../types';

/**
 * Minimal context needed to apply an image update to a single container,
 * independent of whether it comes from the details view or the bulk action.
 * (The backend auto-update daemon mirrors the same routing separately in Go.)
 */
export interface ContainerUpdateContext {
  id: ContainerId;
  /** Display name, used for success/error toasts. */
  name: string;
  /** Current image reference, used to detect un-pullable (sha256) images. */
  image: string;
  /** Docker labels, used to detect compose/stack membership. */
  labels?: Record<string, string>;
  environmentId: EnvironmentId;
  /** Swarm/agent node hosting the container, threaded through to the API. */
  nodeName?: string;
}

/**
 * How a container's image update must be applied:
 * - `standalone`: recreate-with-pull (no compose project).
 * - `stack`: redeploy the owning Portainer stack with re-pull, so the
 *   container stays part of its stack.
 * - `external`: compose-managed but with no matching Portainer stack record;
 *   Portainer must not touch it (would either detach it or drift).
 */
export type ContainerUpdateKind = 'standalone' | 'stack' | 'external';

export interface ContainerUpdatePath {
  kind: ContainerUpdateKind;
  /** Set when `kind === 'stack'`. */
  stackId?: StackId;
  /** Set when `kind === 'stack'`; routes file vs git redeploy. */
  isGitStack?: boolean;
}
