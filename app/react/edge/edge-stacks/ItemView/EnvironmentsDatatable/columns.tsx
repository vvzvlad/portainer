import { CellContext, createColumnHelper } from '@tanstack/react-table';
import { ChevronDown, ChevronRight } from 'lucide-react';
import clsx from 'clsx';
import { useState } from 'react';
import _ from 'lodash';

import { isoDateFromTimestamp } from '@/portainer/filters/filters';
import { getDashboardRoute } from '@/react/portainer/environments/utils';

import { Button } from '@@/buttons';
import { Icon } from '@@/Icon';
import { Link } from '@@/Link';

import { DeploymentStatus, StatusType } from '../../types';

import { EdgeStackEnvironment } from './types';

const columnHelper = createColumnHelper<EdgeStackEnvironment>();

export const columns = _.compact([
  columnHelper.accessor('Name', {
    id: 'name',
    header: 'Name',
    cell({ row: { original: env } }) {
      const { to, params } = getDashboardRoute(env);
      return (
        <Link
          to={to}
          params={params}
          data-cy={`edge-stack-environment-link-${env.Name}`}
        >
          {env.Name}
        </Link>
      );
    },
  }),
  columnHelper.accessor((env) => endpointStatusLabel(env.StackStatus.Status), {
    id: 'status',
    header: 'Status',
    cell({ row: { original: env } }) {
      return (
        <ul className="list-none space-y-2">
          {env.StackStatus.Status.map((s) => (
            <li key={`status-${s.Type}-${s.Time}`}>
              <Status value={s.Type} />
            </li>
          ))}
        </ul>
      );
    },
  }),
  columnHelper.accessor((env) => _.last(env.StackStatus.Status)?.Time, {
    id: 'statusDate',
    header: 'Time',
    cell({ row: { original: env } }) {
      return (
        <ul className="list-none space-y-2">
          {env.StackStatus.Status.map((s) => (
            <li key={`time-${s.Type}-${s.Time}`}>
              {isoDateFromTimestamp(s.Time)}
            </li>
          ))}
        </ul>
      );
    },
  }),
  columnHelper.accessor(
    (env) =>
      env.StackStatus.Status.find((s) => s.Type === StatusType.Error)?.Error,
    {
      id: 'error',
      header: 'Error',
      cell: ErrorCell,
    }
  ),
]);

function ErrorCell({
  getValue,
  row,
}: CellContext<EdgeStackEnvironment, string>) {
  const [isExpanded, setIsExpanded] = useState(false);

  const value = getValue();
  if (!value) {
    return '-';
  }

  return (
    <Button
      color="none"
      className="flex cursor-pointer"
      onClick={() => setIsExpanded(!isExpanded)}
      data-cy={`edge-stack-environment-error-${row.index}`}
    >
      <div className="pr-1 pt-0.5">
        <Icon icon={isExpanded ? ChevronDown : ChevronRight} />
      </div>
      <div
        className={clsx('overflow-hidden whitespace-normal break-keep', {
          'line-clamp-1': !isExpanded,
          'min-w-[150px]': isExpanded,
        })}
      >
        {value}
      </div>
    </Button>
  );
}

function endpointStatusLabel(statusArray: Array<DeploymentStatus>) {
  const labels = [];

  statusArray.forEach((status) => {
    if (status.Type === StatusType.Acknowledged) {
      labels.push('Acknowledged');
    }
    if (status.Type === StatusType.ImagesPulled) {
      labels.push('Images pre-pulled');
    }
    if (status.Type === StatusType.Running) {
      labels.push('Deployed');
    }
    if (status.Type === StatusType.Error) {
      labels.push('Failed');
    }
    if (status.Type === StatusType.PausedDeploying) {
      labels.push('Paused');
    }
    if (status.Type === StatusType.RollingBack) {
      labels.push('Rolling Back');
    }
    if (status.Type === StatusType.RolledBack) {
      labels.push('Rolled Back');
    }
  });

  if (!labels.length) {
    labels.push('Pending');
  }

  return _.uniq(labels).join(', ');
}

function Status({ value }: { value: StatusType }) {
  const color = getStateColor(value);

  return (
    <div className="flex items-center gap-2">
      <span
        className={clsx('h-2 w-2 rounded-full', {
          'bg-orange-5': color === 'orange',
          'bg-green-5': color === 'green',
          'bg-error-5': color === 'red',
        })}
      />

      <span>{_.startCase(StatusType[value])}</span>
    </div>
  );
}

function getStateColor(type: StatusType): 'orange' | 'green' | 'red' {
  switch (type) {
    case StatusType.Acknowledged:
    case StatusType.ImagesPulled:
    case StatusType.DeploymentReceived:
    case StatusType.Running:
    case StatusType.RemoteUpdateSuccess:
    case StatusType.Removed:
      return 'green';
    case StatusType.Error:
      return 'red';
    case StatusType.Pending:
    case StatusType.Deploying:
    case StatusType.Removing:
    case StatusType.PausedDeploying:
    case StatusType.RollingBack:
    case StatusType.RolledBack:
    default:
      return 'orange';
  }
}
