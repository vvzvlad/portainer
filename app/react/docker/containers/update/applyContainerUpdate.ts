import { recreateContainer } from '../containers.service';

import { ContainerUpdateContext } from './types';

/**
 * Shared "apply an image update" primitive. Recreates the single container with
 * a fresh image pull via the Portainer `containers/{id}/recreate` endpoint. This
 * is the single frontend code path behind the "Update now" button and the bulk
 * "Update selected" action, guaranteeing both manual flows behave identically.
 *
 * The recreate endpoint preserves the container's configuration and its compose
 * labels, so a recreated stack member stays part of its project — Watchtower-style:
 * only this one container is updated, the owning stack is never redeployed and an
 * externally-managed compose container is never refused.
 */
export async function applyContainerUpdate(
  context: ContainerUpdateContext,
  { pullImage = true }: { pullImage?: boolean } = {}
): Promise<void> {
  await recreateContainer(context.environmentId, context.id, pullImage, {
    nodeName: context.nodeName,
  });
}
