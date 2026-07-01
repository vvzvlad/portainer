import { Environment } from '@/react/portainer/environments/types';

import { createRowContext } from '@@/datatables/RowContext';

interface RowContextState {
  environment: Environment;
  // When the containers datatable is rendered inside a stack, these are the
  // current stack route params (name/stackId/type/regular/external/orphaned/
  // orphanedRunning/tab). They let the Quick Actions column build links to the
  // stack-scoped container sub-tab states so the stack breadcrumb trail is
  // preserved. Undefined for the global containers list (keeps global links).
  // Raw route params (strings), consumed by buildStackLinkParams downstream.
  stackRouteParams?: Record<string, string | undefined>;
}

const { RowProvider, useRowContext } = createRowContext<RowContextState>();

export { RowProvider, useRowContext };
