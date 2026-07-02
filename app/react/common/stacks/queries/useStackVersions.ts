import { useQuery } from '@tanstack/react-query';

import axios, { parseAxiosError } from '@/portainer/services/axios/axios';
import { EnvironmentId } from '@/react/portainer/environments/types';
import { withError } from '@/react-tools/react-query';

import { StackFileVersionInfo, StackId } from '../types';

import { queryKeys } from './query-keys';

export function useStackVersions(
  stackId?: StackId,
  environmentId?: EnvironmentId,
  { enabled }: { enabled?: boolean } = {}
) {
  return useQuery({
    queryKey: queryKeys.stackVersions(stackId),
    queryFn: ({ signal }) =>
      getStackVersions({
        stackId: stackId!,
        environmentId,
        options: { signal },
      }),

    ...withError('Unable to retrieve stack versions'),
    enabled: !!stackId && enabled,
  });
}

export async function getStackVersions({
  stackId,
  environmentId,
  options = {},
}: {
  stackId: StackId;
  environmentId?: EnvironmentId;
  options?: { signal?: AbortSignal };
}) {
  try {
    const { data } = await axios.get<StackFileVersionInfo[]>(
      `/stacks/${stackId}/versions`,
      {
        params: { endpointId: environmentId },
        signal: options.signal,
      }
    );
    return data;
  } catch (e) {
    throw parseAxiosError(e, 'Unable to retrieve stack versions');
  }
}
