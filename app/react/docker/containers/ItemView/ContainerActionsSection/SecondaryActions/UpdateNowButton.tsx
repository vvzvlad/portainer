import { Download } from 'lucide-react';
import { useRouter } from '@uirouter/react';

import { EnvironmentId } from '@/react/portainer/environments/types';
import { confirmContainerRecreation } from '@/react/docker/containers/ItemView/ConfirmRecreationModal';
import { confirmStackUpdate } from '@/react/common/stacks/common/confirm-stack-update';
import { useStacks } from '@/react/common/stacks/queries/useStacks';
import { notifySuccess } from '@/portainer/services/notifications';
import { useContainerImageStatus } from '@/react/docker/containers/queries/useContainerImageStatus';
import {
  resolveContainerUpdatePath,
  useUpdateContainerImage,
  ContainerUpdateContext,
} from '@/react/docker/containers/update';

import { LoadingButton } from '@@/buttons';
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
  const stacksQuery = useStacks();
  const updateMutation = useUpdateContainerImage();

  // Only meaningful when a newer image is actually available.
  if (statusQuery.data?.Status !== 'outdated') {
    return null;
  }

  const stacks = stacksQuery.data ?? [];
  const path = resolveContainerUpdatePath({ labels, environmentId }, stacks);
  const isExternal = path.kind === 'external';

  const button = (
    <LoadingButton
      color="primary"
      size="small"
      onClick={handleClick}
      disabled={isPortainer || isExternal || stacksQuery.isLoading}
      isLoading={updateMutation.isLoading}
      loadingText="Updating..."
      data-cy="update-now-button"
      icon={Download}
    >
      Update now
    </LoadingButton>
  );

  if (isExternal) {
    return (
      <TooltipWithChildren message="This container belongs to a compose project that is managed outside Portainer, so it can't be updated from here.">
        {button}
      </TooltipWithChildren>
    );
  }

  return button;

  async function handleClick() {
    const context: ContainerUpdateContext = {
      id: containerId,
      name: containerName,
      image: containerImage,
      labels,
      environmentId,
      nodeName,
    };

    let pullImage: boolean;

    if (path.kind === 'stack') {
      const result = await confirmStackUpdate(
        'This will redeploy the stack pulling the latest images and may cause a service interruption. Do you wish to continue?',
        true
      );
      if (!result) {
        return;
      }
      pullImage = result.repullImageAndRedeploy;
    } else {
      const cannotPullImage =
        !containerImage || containerImage.toLowerCase().startsWith('sha256');
      const result = await confirmContainerRecreation(cannotPullImage);
      if (!result) {
        return;
      }
      pullImage = result.pullLatest;
    }

    updateMutation.mutate(
      { context, stacks, pullImage },
      {
        onSuccess: () => {
          notifySuccess('Success', 'Container image update applied');
          router.stateService.go('docker.containers', {}, { reload: true });
        },
      }
    );
  }
}
