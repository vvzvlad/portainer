import { Crumb } from '@@/PageHeader/Breadcrumbs/Breadcrumbs';

// ui-router state name for a container opened from within a stack. Kept as a
// single source of truth so a future state rename stays in sync with the
// breadcrumb logic (and its test) instead of silently falling back to the
// default crumb. The container attribute sub-tabs (logs/stats/inspect/exec/
// attach) are registered as child states of this one so the stack params are
// inherited and the stack trail can be preserved on every sub-tab.
export const STACK_CONTAINER_STATE_NAME = 'docker.stacks.stack.container';

// Shape of the params handed to the docker.stacks.stack route when building the
// back-to-stack breadcrumb link. Values are strings (inherited route params)
// except `external`, which is emitted as a boolean and serialized to
// `external=true` by ui-router.
export type StackLinkParams = {
  name?: string;
  stackId?: string;
  type?: string;
  regular?: string;
  orphaned?: string;
  orphanedRunning?: string;
  external?: boolean;
  tab?: string;
};

// Params for the back-link to the stack-scoped container detail view: the stack
// params (so the stack tree stays active) plus the container id/nodeName.
export type StackContainerLinkParams = StackLinkParams & {
  id?: string;
  nodeName?: string;
};

// True when the current route is the stack-scoped container detail view or any
// of its attribute sub-tabs (which live under STACK_CONTAINER_STATE_NAME). Used
// so the breadcrumb logic recognises the sub-tabs as "opened from a stack"
// instead of falling back to the global Containers trail.
export function isStackContainerState(stateName: string | undefined): boolean {
  return (
    stateName === STACK_CONTAINER_STATE_NAME ||
    !!stateName?.startsWith(`${STACK_CONTAINER_STATE_NAME}.`)
  );
}

// When a container is opened from a stack (state STACK_CONTAINER_STATE_NAME),
// keep the stack trail in the breadcrumbs so the user can navigate back to the
// stack. Otherwise fall back to the global containers list.
export function getContainerBreadcrumbs(
  stateName: string | undefined,
  params: Record<string, string | undefined>,
  containerName: string
): Array<Crumb | string> {
  if (isStackContainerState(stateName)) {
    return [
      { label: 'Stacks', link: 'docker.stacks' },
      {
        label: params.name || '',
        link: 'docker.stacks.stack',
        linkParams: buildStackLinkParams(params),
      },
      containerName,
    ];
  }

  return [{ label: 'Containers', link: 'docker.containers' }, containerName];
}

// Breadcrumbs for a container attribute sub-tab (Logs/Stats/Inspect/Console/
// Attach). Mirrors getContainerBreadcrumbs but adds a clickable container crumb
// (back to the container detail view) plus the tab label, so the stack trail is
// preserved on every sub-tab when (and only when) the container was opened from
// a stack.
export function getContainerSubTabBreadcrumbs(
  stateName: string | undefined,
  params: Record<string, string | undefined>,
  containerName: string,
  tabLabel: string
): Array<Crumb | string> {
  const containerId = params.id;

  if (isStackContainerState(stateName)) {
    return [
      { label: 'Stacks', link: 'docker.stacks' },
      {
        label: params.name || '',
        link: 'docker.stacks.stack',
        linkParams: buildStackLinkParams(params),
      },
      {
        label: containerName,
        link: STACK_CONTAINER_STATE_NAME,
        linkParams: buildStackContainerLinkParams(params, containerId),
      },
      tabLabel,
    ];
  }

  return [
    { label: 'Containers', link: 'docker.containers' },
    {
      label: containerName,
      link: 'docker.containers.container',
      linkParams: { id: containerId },
    },
    tabLabel,
  ];
}

// Rebuild the params expected by the docker.stacks.stack route from the current
// (container) route params, which are inherited from the parent stack state.
export function buildStackLinkParams(
  params: Record<string, string | undefined>
): StackLinkParams {
  // External stacks have no DB id; they are identified by name/type only.
  if (params.external === 'true') {
    return {
      name: params.name,
      type: params.type,
      external: true,
      tab: params.tab,
    };
  }

  return {
    name: params.name,
    stackId: params.stackId,
    type: params.type,
    regular: params.regular,
    orphaned: params.orphaned,
    orphanedRunning: params.orphanedRunning,
    tab: params.tab,
  };
}

// Params for navigating to / linking back to the stack-scoped container detail
// view: the stack params (so the stack tree stays active and the trail can be
// rebuilt) plus the container id/nodeName.
export function buildStackContainerLinkParams(
  params: Record<string, string | undefined>,
  containerId: string | undefined
): StackContainerLinkParams {
  return {
    ...buildStackLinkParams(params),
    id: containerId,
    nodeName: params.nodeName,
  };
}
