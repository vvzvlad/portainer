import { Download } from 'lucide-react';
import { useRouter } from '@uirouter/react';

import { EnvironmentId } from '@/react/portainer/environments/types';
import { useContainerImageStatus } from '@/react/docker/containers/queries/useContainerImageStatus';
import { useApplyContainerImageUpdate } from '@/react/docker/containers/update';

import { ButtonGroup, LoadingButton } from '@@/buttons';
import { TooltipWithChildren } from '@@/Tip/TooltipWithChildren';

import { ContainerId } from '../../../types';

interface UpdateNowButtonProps {
  environmentId: EnvironmentId;
  containerId: ContainerId;
  nodeName?: string;
  containerImage: string;
  containerName: string;
  labels?: Record<string, string>;
  isPortainer: boolean;
}

/**
 * "Update now" surfaces a discoverable per-container apply action ONLY when the
 * image is `outdated`. It routes through the shared update primitive:
 * standalone -> recreate-with-pull, stack-managed -> stack redeploy-with-pull
 * (container stays in its stack). Externally-managed compose containers are
 * shown disabled with an explanatory tooltip, never recreated out-of-band.
 *
 * A stack redeploy is gated by `PortainerStackUpdate` (as everywhere else in the
 * app): a user with container-create but without stack-update rights sees the
 * button disabled with a tooltip rather than getting a 403 on click.
 */
export function UpdateNowButton({
  environmentId,
  containerId,
  nodeName,
  containerImage,
  containerName,
  labels,
  isPortainer,
}: UpdateNowButtonProps) {
  const router = useRouter();
  const statusQuery = useContainerImageStatus(
    environmentId,
    containerId,
    nodeName
  );
  const { apply, isLoading, isExternal, stackUpdateForbidden, canApply } =
    useApplyContainerImageUpdate({
      environmentId,
      containerId,
      nodeName,
      containerImage,
      containerName,
      labels,
      isPortainer,
      // The details view reloads to reflect the recreated/redeployed container.
      onSuccess: () =>
        router.stateService.go('docker.containers', {}, { reload: true }),
    });

  // Only meaningful when a newer image is actually available.
  if (statusQuery.data?.Status !== 'outdated') {
    return null;
  }

  const button = (
    <LoadingButton
      color="primary"
      size="small"
      onClick={apply}
      disabled={!canApply}
      isLoading={isLoading}
      loadingText="Updating..."
      data-cy="update-now-button"
      icon={Download}
    >
      Update now
    </LoadingButton>
  );

  if (isExternal) {
    return (
      <ButtonGroup>
        <TooltipWithChildren message="This container belongs to a compose project that is managed outside Portainer, so it can't be updated from here.">
          {button}
        </TooltipWithChildren>
      </ButtonGroup>
    );
  }

  if (stackUpdateForbidden) {
    return (
      <ButtonGroup>
        <TooltipWithChildren message="Updating this container redeploys its stack, which requires stack update permission you don't have.">
          {button}
        </TooltipWithChildren>
      </ButtonGroup>
    );
  }

  return <ButtonGroup>{button}</ButtonGroup>;
}
