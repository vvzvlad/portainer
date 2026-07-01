import { Stack, StackType } from '@/react/common/stacks/types';
import { COMPOSE_STACK_NAME_LABEL } from '@/react/constants';

import { resolveContainerUpdatePath } from './resolveContainerUpdatePath';

function buildStack(overrides: Partial<Stack>): Stack {
  return {
    Id: 1,
    Name: 'my-stack',
    EndpointId: 3,
    Type: StackType.DockerCompose,
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

  it('flags git stacks (non-zero WorkflowID) so they redeploy via the git path', () => {
    const stack = buildStack({
      Id: 9,
      Name: 'git-stack',
      EndpointId: 3,
      WorkflowID: 42,
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

  it('does not flag a git stack from a deprecated GitConfig alone (no WorkflowID)', () => {
    // GitConfig is deprecated and can diverge from the Workflow/Source model;
    // only a non-zero WorkflowID marks a stack git-backed (mirrors the Go daemon).
    const stack = buildStack({
      Id: 11,
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
    ).toEqual({ kind: 'stack', stackId: 11, isGitStack: false });
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

  it('returns external when a same-named stack is not a compose stack', () => {
    const stack = buildStack({
      Name: 'my-stack',
      EndpointId: 3,
      Type: StackType.Kubernetes,
    });

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
