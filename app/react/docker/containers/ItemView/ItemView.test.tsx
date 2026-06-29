import { render, screen } from '@testing-library/react';
import { http, HttpResponse } from 'msw';

import { withUserProvider } from '@/react/test-utils/withUserProvider';
import { withTestRouter } from '@/react/test-utils/withRouter';
import { withTestQueryProvider } from '@/react/test-utils/withTestQuery';
import { createMockContainer, createMockUser } from '@/react-tools/test-mocks';
import { server } from '@/setup-tests/server';
import { User } from '@/portainer/users/types';
import { ContainerDetailsViewModel } from '@/docker/models/containerDetails';

import { ItemView, STACK_CONTAINER_STATE_NAME } from './ItemView';

const { useCurrentStateAndParamsMock } = vi.hoisted(() => ({
  useCurrentStateAndParamsMock: vi.fn(),
}));

vi.mock('@uirouter/react', async (importOriginal: () => Promise<object>) => ({
  ...(await importOriginal()),
  useCurrentStateAndParams: useCurrentStateAndParamsMock,
}));

describe('ItemView', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    // Default: container opened from the global containers list.
    useCurrentStateAndParamsMock.mockReturnValue({
      state: { name: 'docker.containers.container' },
      params: { id: 'container-id-123', endpointId: '1', nodeName: undefined },
    });
  });

  it('renders page header with container details title', async () => {
    renderComponent();

    expect(
      await screen.findByRole('heading', {
        name: 'Container details',
        level: 1,
      })
    ).toBeVisible();
  });

  it('displays container name in breadcrumbs with leading slash trimmed', async () => {
    renderComponent();

    expect(await screen.findByText('test-container')).toBeVisible();
    expect(screen.queryByText('/test-container')).not.toBeInTheDocument();
    // The global Containers crumb is shown in the default (non-stack) context.
    expect(screen.getByRole('link', { name: 'Containers' })).toBeVisible();
  });

  it('keeps the stack trail when the container is opened from a stack', async () => {
    useCurrentStateAndParamsMock.mockReturnValue({
      state: { name: STACK_CONTAINER_STATE_NAME },
      params: {
        id: 'container-id-123',
        endpointId: '1',
        nodeName: undefined,
        name: 'my-stack',
        stackId: '7',
        type: '2',
        regular: 'true',
      },
    });

    renderComponent();

    // Stack trail is shown instead of the global Containers crumb.
    const stacksCrumb = await screen.findByRole('link', { name: 'Stacks' });
    expect(stacksCrumb).toBeVisible();

    const stackCrumb = screen.getByRole('link', { name: 'my-stack' });
    expect(stackCrumb).toBeVisible();
    // Back-link carries the numeric stack id (stackId) so the stack can reload.
    expect(stackCrumb.getAttribute('href')).toContain('stackId=7');

    expect(
      screen.queryByRole('link', { name: 'Containers' })
    ).not.toBeInTheDocument();
  });

  it('keeps the stack trail without a stack id for an external stack', async () => {
    useCurrentStateAndParamsMock.mockReturnValue({
      state: { name: STACK_CONTAINER_STATE_NAME },
      params: {
        id: 'container-id-123',
        endpointId: '1',
        nodeName: undefined,
        name: 'ext-stack',
        type: '2',
        external: 'true',
        // stackId/regular are present in the inherited route params but the
        // external branch must NOT propagate them. They are set here to
        // non-empty values so the `not.toContain` assertions below actually
        // catch a fall-through-to-regular regression instead of passing vacuously.
        stackId: '7',
        regular: 'true',
        tab: 'logs',
      },
    });

    renderComponent();

    // Stack trail is shown instead of the global Containers crumb.
    const stacksCrumb = await screen.findByRole('link', { name: 'Stacks' });
    expect(stacksCrumb).toBeVisible();

    const stackCrumb = screen.getByRole('link', { name: 'ext-stack' });
    expect(stackCrumb).toBeVisible();
    // External stacks have no DB id, so the back-link is built from
    // name/type/external only and must omit stackId and regular.
    const href = stackCrumb.getAttribute('href');
    expect(href).toContain('external=true');
    expect(href).toContain('type=2');
    expect(href).not.toContain('stackId=');
    expect(href).not.toContain('regular=');

    expect(
      screen.queryByRole('link', { name: 'Containers' })
    ).not.toBeInTheDocument();
  });

  it('keeps the stack trail with the stack id for an orphaned stack', async () => {
    useCurrentStateAndParamsMock.mockReturnValue({
      state: { name: STACK_CONTAINER_STATE_NAME },
      params: {
        id: 'container-id-123',
        endpointId: '1',
        nodeName: undefined,
        name: 'orphan-stack',
        stackId: '7',
        type: '2',
        // Orphaned stacks are identified by orphaned=true and, unlike regular
        // stacks, carry no `regular` flag. They still have a DB id (stackId),
        // so the back-link must keep stackId and must NOT take the external
        // (id-less) branch.
        orphaned: 'true',
        tab: 'logs',
      },
    });

    renderComponent();

    // Stack trail is shown instead of the global Containers crumb.
    const stacksCrumb = await screen.findByRole('link', { name: 'Stacks' });
    expect(stacksCrumb).toBeVisible();

    const stackCrumb = screen.getByRole('link', { name: 'orphan-stack' });
    expect(stackCrumb).toBeVisible();
    const href = stackCrumb.getAttribute('href');
    // Orphaned stacks keep their numeric stack id so the stack can reload,
    // flag the orphaned state, and must not be treated as external.
    expect(href).toContain('stackId=7');
    expect(href).toContain('orphaned=true');
    expect(href).not.toContain('external=');

    expect(
      screen.queryByRole('link', { name: 'Containers' })
    ).not.toBeInTheDocument();
  });

  it('renders health status section when container has health data', async () => {
    renderComponent({
      container: {
        State: {
          Health: {
            Status: 'healthy',
            FailingStreak: 0,
            Log: [
              {
                Start: '2024-01-01T00:00:00Z',
                End: '2024-01-01T00:00:01Z',
                ExitCode: 0,
                Output: 'Health check passed',
              },
            ],
          },
        },
      },
    });

    expect(await screen.findByText('Container health')).toBeVisible();
  });
});

function renderComponent({
  user = createMockUser({ Role: 1 }),
  container,
}: {
  user?: User;
  container?: Partial<ContainerDetailsViewModel>;
} = {}) {
  server.use(
    http.get('/api/endpoints/:endpointId/docker/containers/:id/json', () =>
      HttpResponse.json(createMockContainer(container))
    )
  );

  const Wrapped = withTestQueryProvider(
    withTestRouter(withUserProvider(ItemView, user), {
      stateConfig: [
        { name: 'docker', url: '/docker' },
        { name: 'docker.stacks', url: '/stacks' },
        {
          name: 'docker.stacks.stack',
          url: '/:name?stackId&type&regular&external&orphaned&orphanedRunning&tab',
        },
        { name: 'docker.containers', url: '/containers' },
      ],
    })
  );

  return render(<Wrapped />);
}
