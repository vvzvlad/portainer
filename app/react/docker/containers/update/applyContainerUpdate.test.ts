import { vi, describe, it, expect, beforeEach } from 'vitest';

import { Stack, StackType } from '@/react/common/stacks/types';

import {
  applyContainerUpdate,
  EXTERNAL_STACK_UPDATE_ERROR,
} from './applyContainerUpdate';
import { ContainerUpdateContext } from './types';
import { resolveContainerUpdatePath } from './resolveContainerUpdatePath';

// Mock the side-effecting mutations and the file fetch so we assert the dispatch
// and payload mapping without touching the network.
const recreateContainer = vi.fn();
const updateStack = vi.fn();
const updateGitStack = vi.fn();
const getStackFile = vi.fn();

vi.mock('../containers.service', () => ({
  recreateContainer: (...args: unknown[]) => recreateContainer(...args),
}));
vi.mock('@/react/docker/stacks/useUpdateStack', () => ({
  updateStack: (...args: unknown[]) => updateStack(...args),
}));
vi.mock('@/react/portainer/gitops/queries/useUpdateGitStack', () => ({
  updateGitStack: (...args: unknown[]) => updateGitStack(...args),
}));
vi.mock('@/react/common/stacks/queries/useStackFile', () => ({
  getStackFile: (...args: unknown[]) => getStackFile(...args),
}));

// Drive the standalone/stack/external dispatch deterministically; the resolver's
// own routing logic is covered by resolveContainerUpdatePath.test.ts.
vi.mock('./resolveContainerUpdatePath', () => ({
  resolveContainerUpdatePath: vi.fn(),
}));

const resolveMock = vi.mocked(resolveContainerUpdatePath);

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

function buildStack(overrides: Partial<Stack>): Stack {
  return {
    Id: 7,
    Name: 'my-stack',
    EndpointId: 3,
    Type: StackType.DockerCompose,
    Env: [{ name: 'FOO', value: 'bar' }],
    Option: { Prune: true },
    ...overrides,
  } as Stack;
}

describe('applyContainerUpdate', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it('recreates a standalone container with pull and node, returning "standalone"', async () => {
    resolveMock.mockReturnValue({ kind: 'standalone' });

    const context = buildContext();
    const result = await applyContainerUpdate(context, []);

    expect(result).toBe('standalone');
    expect(recreateContainer).toHaveBeenCalledWith(3, 'abc123', true, {
      nodeName: 'node-1',
    });
    expect(updateStack).not.toHaveBeenCalled();
    expect(updateGitStack).not.toHaveBeenCalled();
  });

  it('passes pullImage=false through to the standalone recreate', async () => {
    resolveMock.mockReturnValue({ kind: 'standalone' });

    await applyContainerUpdate(buildContext(), [], { pullImage: false });

    expect(recreateContainer).toHaveBeenCalledWith(3, 'abc123', false, {
      nodeName: 'node-1',
    });
  });

  it('redeploys a git stack via updateGitStack with RepullImageAndRedeploy', async () => {
    const stack = buildStack({
      Id: 9,
      GitConfig: { URL: 'https://example.com/repo.git' } as Stack['GitConfig'],
    });
    resolveMock.mockReturnValue({
      kind: 'stack',
      stackId: 9,
      isGitStack: true,
    });

    const result = await applyContainerUpdate(buildContext(), [stack]);

    expect(result).toBe('stack');
    expect(updateGitStack).toHaveBeenCalledWith(9, 3, {
      RepullImageAndRedeploy: true,
      Env: [{ name: 'FOO', value: 'bar' }],
      Prune: true,
    });
    expect(getStackFile).not.toHaveBeenCalled();
    expect(updateStack).not.toHaveBeenCalled();
  });

  it('redeploys a file stack via updateStack, preserving its current file content', async () => {
    const stack = buildStack({ Id: 7 });
    resolveMock.mockReturnValue({
      kind: 'stack',
      stackId: 7,
      isGitStack: false,
    });
    getStackFile.mockResolvedValue({ StackFileContent: 'version: "3"\n' });

    const result = await applyContainerUpdate(buildContext(), [stack]);

    expect(result).toBe('stack');
    expect(getStackFile).toHaveBeenCalledWith({ stackId: 7 });
    expect(updateStack).toHaveBeenCalledWith({
      stackId: 7,
      environmentId: 3,
      payload: {
        stackFileContent: 'version: "3"\n',
        env: [{ name: 'FOO', value: 'bar' }],
        prune: true,
        repullImageAndRedeploy: true,
      },
    });
    expect(updateGitStack).not.toHaveBeenCalled();
  });

  it('throws for an externally-managed compose container', async () => {
    resolveMock.mockReturnValue({ kind: 'external' });

    await expect(applyContainerUpdate(buildContext(), [])).rejects.toThrow(
      EXTERNAL_STACK_UPDATE_ERROR
    );
    expect(recreateContainer).not.toHaveBeenCalled();
    expect(updateStack).not.toHaveBeenCalled();
    expect(updateGitStack).not.toHaveBeenCalled();
  });

  it('throws rather than bare-recreating when the resolved stack is missing', async () => {
    // Guard: resolve routed to a stack, but no stack with that id is present.
    resolveMock.mockReturnValue({ kind: 'stack', stackId: 999 });

    await expect(applyContainerUpdate(buildContext(), [])).rejects.toThrow(
      EXTERNAL_STACK_UPDATE_ERROR
    );
    expect(recreateContainer).not.toHaveBeenCalled();
    expect(updateStack).not.toHaveBeenCalled();
    expect(updateGitStack).not.toHaveBeenCalled();
  });
});
