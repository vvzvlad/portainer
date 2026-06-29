import { useQuery } from '@tanstack/react-query';

import axios, { parseAxiosError } from '@/portainer/services/axios/axios';
import { EnvironmentId } from '@/react/portainer/environments/types';

import { buildDockerUrl } from '../../queries/utils/buildDockerUrl';
import { ContainerId } from '../types';

import { queryKeys } from './query-keys';

/**
 * Status of a running container image as reported by the CE detection engine.
 *
 * - `outdated`: a newer image is available in the registry.
 * - `updated`: the local image matches the remote registry digest.
 * - `skipped`: detection is not applicable (digest-pinned or local-only image).
 * - `processing` / `preparing`: detection is in progress (mostly relevant for services).
 * - `error`: detection failed (registry unreachable, auth failure, ...).
 */
export type ContainerImageStatusValue =
  | 'outdated'
  | 'updated'
  | 'skipped'
  | 'processing'
  | 'preparing'
  | 'error';

export interface ContainerImageStatus {
  Status: ContainerImageStatusValue;
  Message?: string;
}

// The backend caches detection results for 24h, so a generous client-side staleTime
// is enough and avoids hammering the endpoint when many rows are visible at once.
const STALE_TIME = 5 * 60 * 1000; // 5 minutes

export async function getContainerImageStatus(
  environmentId: EnvironmentId,
  containerId: ContainerId,
  nodeName?: string
) {
  try {
    const { data } = await axios.get<ContainerImageStatus>(
      buildDockerUrl(environmentId, 'containers', containerId, 'image_status'),
      { params: nodeName ? { nodeName } : undefined }
    );
    return data;
  } catch (err) {
    throw parseAxiosError(err as Error, 'Unable to retrieve image status');
  }
}

export function useContainerImageStatus(
  environmentId: EnvironmentId,
  containerId: ContainerId,
  nodeName?: string,
  enabled = true
) {
  return useQuery(
    queryKeys.imageStatus(environmentId, containerId, nodeName),
    () => getContainerImageStatus(environmentId, containerId, nodeName),
    {
      enabled,
      staleTime: STALE_TIME,
      refetchOnWindowFocus: false,
    }
  );
}
