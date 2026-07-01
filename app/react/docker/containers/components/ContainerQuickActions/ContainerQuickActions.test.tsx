import { render } from '@testing-library/react';
import { ReactNode } from 'react';
import { vi } from 'vitest';

import { ContainerStatus } from '@/react/docker/containers/types';
import { buildStackContainerLinkParams } from '@/react/docker/containers/ItemView/containerBreadcrumbs';

import {
  ContainerQuickActions,
  QuickActionsState,
} from './ContainerQuickActions';

// Render the Link's target state and params as data attributes so the tests can
// assert on them without a full ui-router state tree.
vi.mock('@@/Link', () => ({
  Link: ({
    children,
    to,
    params,
    'data-cy': dataCy,
  }: {
    children: ReactNode;
    to: string;
    params?: Record<string, unknown>;
    'data-cy'?: string;
  }) => (
    <a
      href="/"
      data-cy={dataCy}
      data-to={to}
      data-params={JSON.stringify(params)}
    >
      {children}
    </a>
  ),
}));

// Authorizations are covered elsewhere; render the children unconditionally.
vi.mock('@/react/hooks/useUser', () => ({
  Authorized: ({ children }: { children: ReactNode }) => children,
}));

const allOn: QuickActionsState = {
  showQuickActionAttach: true,
  showQuickActionExec: true,
  showQuickActionInspect: true,
  showQuickActionLogs: true,
  showQuickActionStats: true,
};

const containerId = 'abc123';
const nodeName = 'node-1';

const tabs = ['logs', 'inspect', 'stats', 'exec', 'attach'] as const;

// Look the rendered Link up by the data-cy our Link mock sets.
function byDataCy(tab: (typeof tabs)[number]) {
  return document.querySelector<HTMLElement>(
    `[data-cy="container-${tab}-${containerId}"]`
  );
}

test('without stack context, links target the global container states', () => {
  render(
    <ContainerQuickActions
      containerId={containerId}
      nodeName={nodeName}
      status={ContainerStatus.Running}
      state={allOn}
    />
  );

  tabs.forEach((tab) => {
    const link = byDataCy(tab);
    expect(link).not.toBeNull();
    expect(link?.getAttribute('data-to')).toBe(
      `docker.containers.container.${tab}`
    );
    expect(JSON.parse(link?.getAttribute('data-params') || '{}')).toEqual({
      id: containerId,
      nodeName,
    });
  });
});

test('with stack context, links target the stack-scoped container states with stack params', () => {
  const stackRouteParams = {
    name: 'my-stack',
    stackId: '7',
    type: '2',
    regular: 'true',
    tab: 'containers',
  };
  const stackLinkParams = buildStackContainerLinkParams(
    { ...stackRouteParams, nodeName },
    containerId
  );

  render(
    <ContainerQuickActions
      containerId={containerId}
      nodeName={nodeName}
      status={ContainerStatus.Running}
      state={allOn}
      stackLinkParams={stackLinkParams}
    />
  );

  tabs.forEach((tab) => {
    const link = byDataCy(tab);
    expect(link).not.toBeNull();
    expect(link?.getAttribute('data-to')).toBe(
      `docker.stacks.stack.container.${tab}`
    );
    // Every sub-tab shares the same stack params + container id/nodeName so the
    // stack breadcrumb trail is preserved on navigation.
    expect(JSON.parse(link?.getAttribute('data-params') || '{}')).toEqual({
      name: 'my-stack',
      stackId: '7',
      type: '2',
      regular: 'true',
      orphaned: undefined,
      orphanedRunning: undefined,
      tab: 'containers',
      id: containerId,
      nodeName,
    });
  });
});
