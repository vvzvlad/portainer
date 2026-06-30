import { FileText, Info, BarChart2, Terminal, Paperclip } from 'lucide-react';
import { useCurrentStateAndParams } from '@uirouter/react';

import { ContainerId } from '@/react/docker/containers/types';
import { useAuthorizations } from '@/react/hooks/useUser';

import { Icon } from '@@/Icon';
import { Button, ButtonGroup } from '@@/buttons';
import { Link } from '@@/Link';

import {
  STACK_CONTAINER_STATE_NAME,
  buildStackContainerLinkParams,
  isStackContainerState,
} from '../containerBreadcrumbs';

interface Props {
  containerId: ContainerId;
}

export function ActionLinksRow({ containerId }: Props) {
  const { state, params } = useCurrentStateAndParams();

  // When the container was opened from a stack, keep the sub-tab links inside
  // the stack tree (docker.stacks.stack.container.*) so the stack params are
  // preserved and the breadcrumb keeps the stack trail. Otherwise use the
  // global container states.
  const fromStack = isStackContainerState(state?.name);
  const baseState = fromStack
    ? STACK_CONTAINER_STATE_NAME
    : 'docker.containers.container';
  const linkParams = fromStack
    ? buildStackContainerLinkParams(params, containerId)
    : { id: containerId };

  const { authorized: canLogs } = useAuthorizations(['DockerContainerLogs']);
  const { authorized: canInspect } = useAuthorizations([
    'DockerContainerInspect',
  ]);
  const { authorized: canStats } = useAuthorizations(['DockerContainerStats']);
  const { authorized: canExec } = useAuthorizations(['DockerExecStart']);
  const { authorized: canAttach } = useAuthorizations([
    'DockerContainerAttach',
  ]);

  const hasAnyAuthorization =
    canLogs || canInspect || canStats || canExec || canAttach;

  if (!hasAnyAuthorization) {
    return null;
  }

  return (
    <tr>
      <td colSpan={2}>
        <ButtonGroup>
          {canLogs && (
            <Button
              as={Link}
              props={{
                to: `${baseState}.logs`,
                params: linkParams,
              }}
              data-cy="container-logs-link"
              color="link"
            >
              <Icon icon={FileText} className="lucide space-right" />
              Logs
            </Button>
          )}
          {canInspect && (
            <Button
              as={Link}
              props={{
                to: `${baseState}.inspect`,
                params: linkParams,
              }}
              data-cy="container-inspect-link"
              color="link"
            >
              <Icon icon={Info} className="lucide space-right" />
              Inspect
            </Button>
          )}
          {canStats && (
            <Button
              as={Link}
              props={{
                to: `${baseState}.stats`,
                params: linkParams,
              }}
              data-cy="container-stats-link"
              color="link"
            >
              <Icon icon={BarChart2} className="lucide space-right" />
              Stats
            </Button>
          )}
          {canExec && (
            <Button
              as={Link}
              props={{
                to: `${baseState}.exec`,
                params: linkParams,
              }}
              data-cy="container-console-link"
              color="link"
            >
              <Icon icon={Terminal} className="lucide space-right" />
              Console
            </Button>
          )}
          {canAttach && (
            <Button
              as={Link}
              props={{
                to: `${baseState}.attach`,
                params: linkParams,
              }}
              data-cy="container-attach-link"
              color="link"
            >
              <Icon icon={Paperclip} className="lucide space-right" />
              Attach
            </Button>
          )}
        </ButtonGroup>
      </td>
    </tr>
  );
}
