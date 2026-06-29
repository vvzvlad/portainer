import { Crumb } from '@@/PageHeader/Breadcrumbs/Breadcrumbs';

import {
  STACK_CONTAINER_STATE_NAME,
  getContainerBreadcrumbs,
  getContainerSubTabBreadcrumbs,
  isStackContainerState,
} from './containerBreadcrumbs';

// Narrow a crumb to the object form so the tests can assert on its link/params.
function asCrumb(value: Crumb | string): Crumb {
  if (typeof value === 'string') {
    throw new Error(`expected an object crumb but got the string "${value}"`);
  }
  return value;
}

// Find a crumb by its visible label.
function findCrumb(
  crumbs: Array<Crumb | string>,
  label: string
): Crumb | undefined {
  return crumbs
    .filter((c): c is Crumb => typeof c !== 'string')
    .find((c) => c.label === label);
}

describe('isStackContainerState', () => {
  it('matches the stack container detail state and its sub-tab children', () => {
    expect(isStackContainerState(STACK_CONTAINER_STATE_NAME)).toBe(true);
    expect(isStackContainerState(`${STACK_CONTAINER_STATE_NAME}.logs`)).toBe(
      true
    );
    expect(isStackContainerState(`${STACK_CONTAINER_STATE_NAME}.exec`)).toBe(
      true
    );
  });

  it('does not match the global container states or undefined', () => {
    expect(isStackContainerState('docker.containers.container')).toBe(false);
    expect(isStackContainerState('docker.containers.container.logs')).toBe(
      false
    );
    expect(isStackContainerState(undefined)).toBe(false);
  });
});

describe('getContainerBreadcrumbs', () => {
  it('returns the global Containers trail when not opened from a stack', () => {
    const crumbs = getContainerBreadcrumbs(
      'docker.containers.container',
      { id: 'c1' },
      'web'
    );

    expect(asCrumb(crumbs[0]).link).toBe('docker.containers');
    expect(crumbs[crumbs.length - 1]).toBe('web');
    expect(findCrumb(crumbs, 'Stacks')).toBeUndefined();
  });

  it('returns the stack trail when opened from a stack', () => {
    const crumbs = getContainerBreadcrumbs(
      STACK_CONTAINER_STATE_NAME,
      { id: 'c1', name: 'my-stack', stackId: '7', type: '2', regular: 'true' },
      'web'
    );

    expect(asCrumb(crumbs[0]).link).toBe('docker.stacks');
    const stackCrumb = asCrumb(crumbs[1]);
    expect(stackCrumb.label).toBe('my-stack');
    expect(stackCrumb.link).toBe('docker.stacks.stack');
    expect(stackCrumb.linkParams).toMatchObject({ stackId: '7' });
    expect(findCrumb(crumbs, 'Containers')).toBeUndefined();
  });
});

describe('getContainerSubTabBreadcrumbs', () => {
  it('falls back to the global Containers trail for a non-stack sub-tab', () => {
    const crumbs = getContainerSubTabBreadcrumbs(
      'docker.containers.container.logs',
      { id: 'c1' },
      'web',
      'Logs'
    );

    expect(crumbs).toHaveLength(3);
    expect(asCrumb(crumbs[0]).link).toBe('docker.containers');
    const containerCrumb = asCrumb(crumbs[1]);
    expect(containerCrumb.label).toBe('web');
    expect(containerCrumb.link).toBe('docker.containers.container');
    expect(containerCrumb.linkParams).toEqual({ id: 'c1' });
    expect(crumbs[2]).toBe('Logs');
    expect(findCrumb(crumbs, 'Stacks')).toBeUndefined();
  });

  it('keeps the stack trail for a regular stack sub-tab', () => {
    const crumbs = getContainerSubTabBreadcrumbs(
      `${STACK_CONTAINER_STATE_NAME}.logs`,
      { id: 'c1', name: 'my-stack', stackId: '7', type: '2', regular: 'true' },
      'web',
      'Logs'
    );

    // Stacks > my-stack > web > Logs, and never the global Containers crumb.
    expect(asCrumb(crumbs[0]).link).toBe('docker.stacks');
    const stackCrumb = asCrumb(crumbs[1]);
    expect(stackCrumb.label).toBe('my-stack');
    expect(stackCrumb.link).toBe('docker.stacks.stack');
    expect(stackCrumb.linkParams).toMatchObject({
      stackId: '7',
      regular: 'true',
    });

    const containerCrumb = asCrumb(crumbs[2]);
    expect(containerCrumb.label).toBe('web');
    // The container crumb links back into the stack tree, carrying both the
    // container id and the stack params so the trail survives a reload.
    expect(containerCrumb.link).toBe(STACK_CONTAINER_STATE_NAME);
    expect(containerCrumb.linkParams).toMatchObject({
      id: 'c1',
      stackId: '7',
    });

    expect(crumbs[crumbs.length - 1]).toBe('Logs');
    expect(findCrumb(crumbs, 'Containers')).toBeUndefined();
  });

  it('omits the stack id and regular flag for an external stack sub-tab', () => {
    const crumbs = getContainerSubTabBreadcrumbs(
      `${STACK_CONTAINER_STATE_NAME}.stats`,
      {
        id: 'c1',
        name: 'ext-stack',
        type: '2',
        external: 'true',
        // Present in the inherited params but must NOT propagate via the
        // external branch.
        stackId: '7',
        regular: 'true',
      },
      'web',
      'Stats'
    );

    const stackCrumb = asCrumb(crumbs[1]);
    expect(stackCrumb.linkParams).toMatchObject({ external: true, type: '2' });
    expect(stackCrumb.linkParams).not.toHaveProperty('stackId');
    expect(stackCrumb.linkParams).not.toHaveProperty('regular');

    const containerCrumb = asCrumb(crumbs[2]);
    expect(containerCrumb.linkParams).toMatchObject({
      id: 'c1',
      external: true,
    });
    expect(containerCrumb.linkParams).not.toHaveProperty('stackId');
    expect(findCrumb(crumbs, 'Containers')).toBeUndefined();
  });

  it('keeps the stack id and flags an orphaned stack sub-tab', () => {
    const crumbs = getContainerSubTabBreadcrumbs(
      `${STACK_CONTAINER_STATE_NAME}.exec`,
      {
        id: 'c1',
        name: 'orphan-stack',
        stackId: '7',
        type: '2',
        orphaned: 'true',
      },
      'web',
      'Console'
    );

    const stackCrumb = asCrumb(crumbs[1]);
    expect(stackCrumb.linkParams).toMatchObject({
      stackId: '7',
      orphaned: 'true',
    });
    expect(stackCrumb.linkParams).not.toHaveProperty('external');

    const containerCrumb = asCrumb(crumbs[2]);
    expect(containerCrumb.linkParams).toMatchObject({
      id: 'c1',
      stackId: '7',
      orphaned: 'true',
    });
    expect(crumbs[crumbs.length - 1]).toBe('Console');
    expect(findCrumb(crumbs, 'Containers')).toBeUndefined();
  });
});
