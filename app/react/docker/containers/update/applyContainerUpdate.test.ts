import { vi, describe, it, expect, beforeEach } from 'vitest';

import { COMPOSE_STACK_NAME_LABEL } from '@/react/constants';

import { applyContainerUpdate } from './applyContainerUpdate';
import { ContainerUpdateContext } from './types';

// Mock the recreate mutation so we assert the dispatch and payload mapping
// without touching the network.
const recreateContainer = vi.fn();

vi.mock('../containers.service', () => ({
  recreateContainer: (...args: unknown[]) => recreateContainer(...args),
}));

function buildContext(
  overrides: Partial<ContainerUpdateContext> = {}
): ContainerUpdateContext {
  return {
    id: 'abc123',
    name: 'my-container',
    image: 'nginx:latest',
    environmentId: 3,
    nodeName: 'node-1',
    ...overrides,
  };
}

describe('applyContainerUpdate', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it('recreates a plain container with pull and node', async () => {
    await applyContainerUpdate(buildContext());

    expect(recreateContainer).toHaveBeenCalledWith(3, 'abc123', true, {
      nodeName: 'node-1',
    });
  });

  it('recreates a compose-managed container the same way (never redeploys a stack)', async () => {
    // A container carrying compose labels must ALSO recreate: the recreate
    // endpoint preserves those labels, so it stays part of its project.
    await applyContainerUpdate(
      buildContext({
        labels: { [COMPOSE_STACK_NAME_LABEL]: 'my-stack' },
      })
    );

    expect(recreateContainer).toHaveBeenCalledWith(3, 'abc123', true, {
      nodeName: 'node-1',
    });
    expect(recreateContainer).toHaveBeenCalledTimes(1);
  });

  it('passes pullImage=false through to the recreate', async () => {
    await applyContainerUpdate(buildContext(), { pullImage: false });

    expect(recreateContainer).toHaveBeenCalledWith(3, 'abc123', false, {
      nodeName: 'node-1',
    });
  });
});
