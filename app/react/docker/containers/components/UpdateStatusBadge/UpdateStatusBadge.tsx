import { Loader } from 'lucide-react';

import UpdatesAvailable from '@/assets/ico/icon_updates-available.svg?c';
import UpToDate from '@/assets/ico/icon_up-to-date.svg?c';

import { Icon } from '@@/Icon';

import { ContainerImageStatusValue } from '../../queries/useContainerImageStatus';

export interface Props {
  status?: ContainerImageStatusValue;
  isLoading?: boolean;
}

/**
 * CE container image update badge.
 *
 * Renders "Update available" when the image is outdated, an up-to-date indicator
 * when it matches the registry, a spinner while the status is being fetched, and
 * nothing for neutral states (skipped/error/processing/preparing) so that
 * undetectable images don't clutter the UI.
 */
export function UpdateStatusBadge({ status, isLoading }: Props) {
  if (isLoading) {
    return (
      <span role="status" aria-label="Checking for image updates">
        <Icon icon={Loader} size="sm" spin className="!mr-1 align-middle" />
      </span>
    );
  }

  if (status === 'outdated') {
    return (
      <span className="inline-flex items-center gap-1 whitespace-nowrap text-warning">
        <Icon icon={UpdatesAvailable} size="sm" className="align-middle" />
        Update available
      </span>
    );
  }

  if (status === 'updated') {
    return (
      <span className="inline-flex items-center gap-1 whitespace-nowrap text-muted">
        <Icon icon={UpToDate} size="sm" className="align-middle" />
        Up to date
      </span>
    );
  }

  // skipped / error / processing / preparing / undefined -> neutral, nothing to show.
  return null;
}
