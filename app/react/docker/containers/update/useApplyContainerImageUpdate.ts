import { EnvironmentId } from '@/react/portainer/environments/types';
import { confirmContainerRecreation } from '@/react/docker/containers/ItemView/ConfirmRecreationModal';
import { confirmStackUpdate } from '@/react/common/stacks/common/confirm-stack-update';
import { useStacks } from '@/react/common/stacks/queries/useStacks';
import { notifySuccess } from '@/portainer/services/notifications';
import { useAuthorizations } from '@/react/hooks/useUser';

import { ContainerId } from '../types';

import { resolveContainerUpdatePath } from './resolveContainerUpdatePath';
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
 * dialogs, standalone/stack/external routing and permission gating live in ONE
 * place.
 *
 * Routing mirrors the backend: standalone -> recreate-with-pull, stack-managed ->
 * stack redeploy-with-pull (container stays in its stack), externally-managed
 * compose -> not touched. A stack redeploy is gated by `PortainerStackUpdate`, so
 * a user with container-create but without stack-update rights can't apply it
 * (surfaced via `stackUpdateForbidden` rather than a 403 on click).
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
  const stacksQuery = useStacks();
  const updateMutation = useUpdateContainerImage();
  // A stack redeploy needs stack-update rights, not just container-create.
  const { authorized: canUpdateStack } = useAuthorizations(
    'PortainerStackUpdate',
    environmentId
  );

  const stacks = stacksQuery.data ?? [];
  const path = resolveContainerUpdatePath({ labels, environmentId }, stacks);
  const isExternal = path.kind === 'external';
  // Stack-managed container the user isn't allowed to redeploy.
  const stackUpdateForbidden = path.kind === 'stack' && !canUpdateStack;
  const canApply =
    !isPortainer &&
    !isExternal &&
    !stackUpdateForbidden &&
    !stacksQuery.isLoading;

  return {
    apply,
    isLoading: updateMutation.isLoading,
    isExternal,
    stackUpdateForbidden,
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
          onSuccess?.();
        },
      }
    );
  }
}
