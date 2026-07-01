import { Loader } from 'lucide-react';
import clsx from 'clsx';

import UpdatesAvailable from '@/assets/ico/icon_updates-available.svg?c';
import UpToDate from '@/assets/ico/icon_up-to-date.svg?c';

import { Icon } from '@@/Icon';

import { ContainerImageStatusValue } from '../../queries/useContainerImageStatus';

export interface Props {
  status?: ContainerImageStatusValue;
  isLoading?: boolean;
  /**
   * When provided, the "Update available" badge becomes a button that triggers
   * an image update. Omit it to keep the plain, non-interactive span.
   */
  onUpdateClick?: () => void;
  /**
   * When provided, the "Up to date" badge becomes a button that re-checks the
   * registry for a newer image. Omit it to keep the plain span.
   */
  onRecheckClick?: () => void;
  /** Drives the spinner shown on the "Up to date" badge while a recheck runs. */
  isRechecking?: boolean;
  /**
   * While an update is in flight the "Update available" button shows a pending
   * "Updating..." affordance and is disabled, preventing a double-submit (parity
   * with the details-view "Update now" button).
   */
  isUpdating?: boolean;
}

const badgeClass = 'inline-flex items-center gap-1 whitespace-nowrap';
// Reset the native button chrome so the interactive badge looks identical to the
// plain span, while staying keyboard-focusable with a visible focus ring.
const interactiveClass =
  'cursor-pointer border-0 bg-transparent p-0 hover:underline focus:outline-none focus-visible:rounded-sm focus-visible:ring-2 focus-visible:ring-blue-5';

/**
 * CE container image update badge.
 *
 * Renders "Update available" when the image is outdated, an up-to-date indicator
 * when it matches the registry, a spinner while the status is being fetched, and
 * nothing for neutral states (skipped/error/processing/preparing) so that
 * undetectable images don't clutter the UI.
 *
 * The two visible states become interactive when the matching handler is passed:
 * clicking "Update available" applies the image update, clicking "Up to date"
 * re-checks the registry (showing a spinner meanwhile). Without a handler each
 * stays a plain span so non-interactive callers are unaffected.
 */
export function UpdateStatusBadge({
  status,
  isLoading,
  onUpdateClick,
  onRecheckClick,
  isRechecking,
  isUpdating,
}: Props) {
  if (isLoading) {
    return (
      <span role="status" aria-label="Checking for image updates">
        <Icon icon={Loader} size="sm" spin className="!mr-1 align-middle" />
      </span>
    );
  }

  if (status === 'outdated') {
    const content = isUpdating ? (
      <>
        <Icon icon={Loader} size="sm" spin className="align-middle" />
        Updating...
      </>
    ) : (
      <>
        <Icon icon={UpdatesAvailable} size="sm" className="align-middle" />
        Update available
      </>
    );

    if (onUpdateClick) {
      return (
        <button
          type="button"
          onClick={onUpdateClick}
          // Disabled while updating: blocks the click so a second submit can't
          // fire another recreate/redeploy.
          disabled={isUpdating}
          className={clsx(badgeClass, interactiveClass, 'text-warning')}
          data-cy="update-status-badge-update"
          aria-label={
            isUpdating
              ? 'Updating the image'
              : 'Update available, click to update the image'
          }
        >
          {content}
        </button>
      );
    }

    return <span className={clsx(badgeClass, 'text-warning')}>{content}</span>;
  }

  if (status === 'updated') {
    const content = (
      <>
        <Icon
          icon={isRechecking ? Loader : UpToDate}
          size="sm"
          spin={isRechecking}
          className="align-middle"
        />
        Up to date
      </>
    );

    if (onRecheckClick) {
      return (
        <button
          type="button"
          onClick={onRecheckClick}
          disabled={isRechecking}
          className={clsx(badgeClass, interactiveClass, 'text-muted')}
          data-cy="update-status-badge-recheck"
          aria-label="Up to date, click to re-check for image updates"
        >
          {content}
        </button>
      );
    }

    return <span className={clsx(badgeClass, 'text-muted')}>{content}</span>;
  }

  // skipped / error / processing / preparing / undefined -> neutral, nothing to show.
  return null;
}
