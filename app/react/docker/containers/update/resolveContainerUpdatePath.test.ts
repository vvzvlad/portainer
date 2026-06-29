import { Stack } from '@/react/common/stacks/types';
import { COMPOSE_STACK_NAME_LABEL } from '@/react/constants';

import { resolveContainerUpdatePath } from './resolveContainerUpdatePath';

function buildStack(overrides: Partial<Stack>): Stack {
  return {
    Id: 1,
    Name: 'my-stack',
    EndpointId: 3,
    ...overrides,
  } as Stack;
}

describe('resolveContainerUpdatePath', () => {
  it('returns standalone when there is no compose project label', () => {
    expect(
      resolveContainerUpdatePath({ labels: {}, environmentId: 3 }, [])
    ).toEqual({ kind: 'standalone' });
  });

  it('returns stack when the compose project matches a Portainer stack', () => {
    const stack = buildStack({ Id: 7, Name: 'my-stack', EndpointId: 3 });

    expect(
      resolveContainerUpdatePath(
        {
          labels: { [COMPOSE_STACK_NAME_LABEL]: 'my-stack' },
          environmentId: 3,
        },
        [stack]
      )
    ).toEqual({ kind: 'stack', stackId: 7, isGitStack: false });
  });

  it('flags git stacks so they redeploy via the git path', () => {
    const stack = buildStack({
      Id: 9,
      Name: 'git-stack',
      EndpointId: 3,
      GitConfig: { URL: 'https://example.com/repo.git' } as Stack['GitConfig'],
    });

    expect(
      resolveContainerUpdatePath(
        {
          labels: { [COMPOSE_STACK_NAME_LABEL]: 'git-stack' },
          environmentId: 3,
        },
        [stack]
      )
    ).toEqual({ kind: 'stack', stackId: 9, isGitStack: true });
  });

  it('returns external when the compose project has no matching stack', () => {
    expect(
      resolveContainerUpdatePath(
        {
          labels: { [COMPOSE_STACK_NAME_LABEL]: 'unknown-project' },
          environmentId: 3,
        },
        [buildStack({ Name: 'other' })]
      )
    ).toEqual({ kind: 'external' });
  });

  it('does not match a stack from a different endpoint', () => {
    const stack = buildStack({ Name: 'my-stack', EndpointId: 99 });

    expect(
      resolveContainerUpdatePath(
        {
          labels: { [COMPOSE_STACK_NAME_LABEL]: 'my-stack' },
          environmentId: 3,
        },
        [stack]
      )
    ).toEqual({ kind: 'external' });
  });
});
