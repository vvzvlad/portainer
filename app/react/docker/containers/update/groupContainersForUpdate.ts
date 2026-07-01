import { Stack, StackId } from '@/react/common/stacks/types';

import { resolveContainerUpdatePath } from './resolveContainerUpdatePath';
import { ContainerUpdateContext } from './types';

export interface GroupedContainerUpdates {
  /** Standalone containers, each recreated individually. */
  standalone: ContainerUpdateContext[];
  /**
   * One entry per owning stack, even if several selected containers belong to
   * the same stack: the stack is redeployed ONCE (mirrors M4's "one redeploy
   * per stack per tick"). `context` is a representative container of the stack.
   */
  stacks: Array<{ stackId: StackId; context: ContainerUpdateContext }>;
  /** Externally-managed compose containers, skipped (never recreated). */
  external: ContainerUpdateContext[];
}

/**
 * Partition a set of containers into the apply paths, de-duplicating stack
 * containers so each stack is redeployed only once regardless of how many of
 * its containers were selected. Pure and unit-testable.
 */
export function groupContainersForUpdate(
  contexts: ContainerUpdateContext[],
  stacks: Stack[]
): GroupedContainerUpdates {
  const standalone: ContainerUpdateContext[] = [];
  const external: ContainerUpdateContext[] = [];
  const stackMap = new Map<
    StackId,
    { stackId: StackId; context: ContainerUpdateContext }
  >();

  contexts.forEach((context) => {
    const path = resolveContainerUpdatePath(context, stacks);

    if (path.kind === 'standalone') {
      standalone.push(context);
    } else if (path.kind === 'external') {
      external.push(context);
    } else if (path.kind === 'stack' && path.stackId != null) {
      if (!stackMap.has(path.stackId)) {
        stackMap.set(path.stackId, { stackId: path.stackId, context });
      }
    }
  });

  return { standalone, external, stacks: Array.from(stackMap.values()) };
}
