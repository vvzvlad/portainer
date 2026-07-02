import { EnvironmentId } from '@/react/portainer/environments/types';
import { confirmContainerRecreation } from '@/react/docker/containers/ItemView/ConfirmRecreationModal';
import { notifySuccess } from '@/portainer/services/notifications';

import { ContainerId } from '../types';

import { useUpdateContainerImage } from './useUpdateContainerImage';
import { ContainerUpdateContext } from './types';

interface Params {
  environmentId: EnvironmentId;
  containerId: ContainerId;
  nodeName?: string;
  containerImage: string;
  containerName: string;
  labels?: Record<string, string>;
  /** Portainer's own container can't update itself, so the action is disabled. */
  isPortainer: boolean;
  /**
   * Post-success side effect. The details-view button navigates/reloads here;
   * the list badge leaves it empty and lets react-query invalidation refresh the
   * row.
   */
  onSuccess?: () => void;
}

/**
 * Shared "apply an image update to a single container" action, used by both the
 * details-view "Update now" button and the interactive list badge so the confirm
 * dialog lives in ONE place.
 *
 * Watchtower-style: always recreates just this one container with a fresh image
 * pull (the recreate endpoint preserves config + compose labels, so a stack
 * member stays part of its project). The only container excluded is Portainer's
 * own, which can't recreate itself.
 */
export function useApplyContainerImageUpdate({
  environmentId,
  containerId,
  nodeName,
  containerImage,
  containerName,
  labels,
  isPortainer,
  onSuccess,
}: Params) {
  const updateMutation = useUpdateContainerImage();

  const canApply = !isPortainer && !updateMutation.isLoading;

  return {
    apply,
    isLoading: updateMutation.isLoading,
    canApply,
  };

  async function apply() {
    const context: ContainerUpdateContext = {
      id: containerId,
      name: containerName,
      image: containerImage,
      labels,
      environmentId,
      nodeName,
    };

    const cannotPullImage =
      !containerImage || containerImage.toLowerCase().startsWith('sha256');
    const result = await confirmContainerRecreation(cannotPullImage);
    if (!result) {
      return;
    }
    const pullImage = result.pullLatest;

    updateMutation.mutate(
      { context, pullImage },
      {
        onSuccess: () => {
          notifySuccess('Success', 'Container image update applied');
          onSuccess?.();
        },
      }
    );
  }
}
