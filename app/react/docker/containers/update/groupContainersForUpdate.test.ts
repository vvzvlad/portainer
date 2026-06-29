import { Stack } from '@/react/common/stacks/types';
import { COMPOSE_STACK_NAME_LABEL } from '@/react/constants';

import { groupContainersForUpdate } from './groupContainersForUpdate';
import { ContainerUpdateContext } from './types';

function buildContext(
  overrides: Partial<ContainerUpdateContext>
): ContainerUpdateContext {
  return {
    id: 'c1',
    name: 'container',
    image: 'nginx:latest',
    environmentId: 3,
    ...overrides,
  };
}

const stack = {
  Id: 7,
  Name: 'my-stack',
  EndpointId: 3,
} as Stack;

describe('groupContainersForUpdate', () => {
  it('redeploys a stack only once when several of its containers are selected', () => {
    const contexts = [
      buildContext({
        id: 'a',
        labels: { [COMPOSE_STACK_NAME_LABEL]: 'my-stack' },
      }),
      buildContext({
        id: 'b',
        labels: { [COMPOSE_STACK_NAME_LABEL]: 'my-stack' },
      }),
    ];

    const result = groupContainersForUpdate(contexts, [stack]);

    expect(result.stacks).toHaveLength(1);
    expect(result.stacks[0].stackId).toBe(7);
    expect(result.standalone).toHaveLength(0);
    expect(result.external).toHaveLength(0);
  });

  it('partitions standalone, stack and external containers', () => {
    const contexts = [
      buildContext({ id: 'standalone', labels: {} }),
      buildContext({
        id: 'stack',
        labels: { [COMPOSE_STACK_NAME_LABEL]: 'my-stack' },
      }),
      buildContext({
        id: 'external',
        labels: { [COMPOSE_STACK_NAME_LABEL]: 'not-in-portainer' },
      }),
    ];

    const result = groupContainersForUpdate(contexts, [stack]);

    expect(result.standalone.map((c) => c.id)).toEqual(['standalone']);
    expect(result.stacks.map((s) => s.stackId)).toEqual([7]);
    expect(result.external.map((c) => c.id)).toEqual(['external']);
  });
});
