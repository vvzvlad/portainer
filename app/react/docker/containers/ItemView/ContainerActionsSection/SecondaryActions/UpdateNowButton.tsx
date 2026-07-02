import { Download } from 'lucide-react';
import { useRouter } from '@uirouter/react';

import { EnvironmentId } from '@/react/portainer/environments/types';
import { useContainerImageStatus } from '@/react/docker/containers/queries/useContainerImageStatus';
import { useApplyContainerImageUpdate } from '@/react/docker/containers/update';

import { ButtonGroup, LoadingButton } from '@@/buttons';

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
 * image is `outdated`. It routes through the shared update primitive, which
 * always recreates just this one container with a fresh image pull (the recreate
 * endpoint preserves config + compose labels, so a stack member stays part of
 * its project). The button is disabled only for Portainer's own container, which
 * can't recreate itself.
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
  const { apply, isLoading, canApply } = useApplyContainerImageUpdate({
    environmentId,
    containerId,
    nodeName,
    containerImage,
    containerName,
    labels,
    isPortainer,
    // The details view reloads to reflect the recreated container.
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

  return <ButtonGroup>{button}</ButtonGroup>;
}
