import { EnvironmentId } from '@/react/portainer/environments/types';

import { ContainerId } from '../types';

/**
 * Minimal context needed to apply an image update to a single container,
 * independent of whether it comes from the details view or the bulk action.
 */
export interface ContainerUpdateContext {
  id: ContainerId;
  /** Display name, used for success/error toasts. */
  name: string;
  /** Current image reference, used to detect un-pullable (sha256) images. */
  image: string;
  /** Docker labels, preserved by the recreate endpoint (kept for callers). */
  labels?: Record<string, string>;
  environmentId: EnvironmentId;
  /** Swarm/agent node hosting the container, threaded through to the API. */
  nodeName?: string;
}
