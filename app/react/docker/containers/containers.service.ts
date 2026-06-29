import _ from 'lodash';
import { InternalAxiosRequestConfig } from 'axios';

import { EnvironmentId } from '@/react/portainer/environments/types';
import PortainerError from '@/portainer/error';
import axios, {
  agentTargetHeader,
  parseAxiosError,
} from '@/portainer/services/axios/axios';
import {
  portainerAgentManagerOperation,
  portainerAgentTargetHeader,
} from '@/portainer/services/http-request.helper';
import { dockerMaxAPIVersionInterceptor } from '@/portainer/services/dockerMaxApiVersionInterceptor';

import { withAgentTargetHeader } from '../proxy/queries/utils';
import { buildDockerProxyUrl } from '../proxy/queries/buildDockerProxyUrl';
import { buildDockerUrl } from '../queries/utils/buildDockerUrl';

import { ContainerId, ContainerLogsParams } from './types';

export async function startContainer(
  environmentId: EnvironmentId,
  id: ContainerId,
  { nodeName }: { nodeName?: string } = {}
) {
  try {
    await axios.post<void>(
      buildDockerProxyUrl(environmentId, 'containers', id, 'start'),
      {},
      {
        headers: { ...withAgentTargetHeader(nodeName) },
      }
    );
  } catch (e) {
    throw parseAxiosError(e, 'Failed starting container');
  }
}

export async function stopContainer(
  endpointId: EnvironmentId,
  id: ContainerId,
  { nodeName }: { nodeName?: string } = {}
) {
  try {
    await axios.post<void>(
      buildDockerProxyUrl(endpointId, 'containers', id, 'stop'),
      {},
      { headers: { ...withAgentTargetHeader(nodeName) } }
    );
  } catch (e) {
    throw parseAxiosError(e, 'Failed stopping container');
  }
}

export async function recreateContainer(
  endpointId: EnvironmentId,
  id: ContainerId,
  pullImage: boolean,
  { nodeName }: { nodeName?: string } = {}
) {
  try {
    await axios.post<void>(
      buildDockerUrl(endpointId, 'containers', id, 'recreate'),
      {
        PullImage: pullImage,
      },
      { headers: { ...withAgentTargetHeader(nodeName) } }
    );
  } catch (e) {
    throw parseAxiosError(e, 'Failed recreating container');
  }
}

export async function restartContainer(
  endpointId: EnvironmentId,
  id: ContainerId,
  { nodeName }: { nodeName?: string } = {}
) {
  try {
    await axios.post<void>(
      buildDockerProxyUrl(endpointId, 'containers', id, 'restart'),
      {},
      { headers: { ...withAgentTargetHeader(nodeName) } }
    );
  } catch (e) {
    throw parseAxiosError(e, 'Failed restarting container');
  }
}

export async function killContainer(
  endpointId: EnvironmentId,
  id: ContainerId,
  { nodeName }: { nodeName?: string } = {}
) {
  try {
    await axios.post<void>(
      buildDockerProxyUrl(endpointId, 'containers', id, 'kill'),
      {},
      { headers: { ...withAgentTargetHeader(nodeName) } }
    );
  } catch (e) {
    throw parseAxiosError(e, 'Failed killing container');
  }
}

export async function pauseContainer(
  endpointId: EnvironmentId,
  id: ContainerId,
  { nodeName }: { nodeName?: string } = {}
) {
  try {
    await axios.post<void>(
      buildDockerProxyUrl(endpointId, 'containers', id, 'pause'),
      {},
      { headers: { ...withAgentTargetHeader(nodeName) } }
    );
  } catch (e) {
    throw parseAxiosError(e, 'Failed pausing container');
  }
}

export async function resumeContainer(
  endpointId: EnvironmentId,
  id: ContainerId,
  { nodeName }: { nodeName?: string } = {}
) {
  try {
    await axios.post<void>(
      buildDockerProxyUrl(endpointId, 'containers', id, 'unpause'),
      {},
      { headers: { ...withAgentTargetHeader(nodeName) } }
    );
  } catch (e) {
    throw parseAxiosError(e, 'Failed resuming container');
  }
}

export async function renameContainer(
  endpointId: EnvironmentId,
  id: ContainerId,
  name: string,
  { nodeName }: { nodeName?: string } = {}
) {
  try {
    await axios.post<void>(
      buildDockerProxyUrl(endpointId, 'containers', id, 'rename'),
      {},
      {
        params: { name },
        headers: { ...withAgentTargetHeader(nodeName) },
      }
    );
  } catch (e) {
    throw parseAxiosError(e, 'Failed renaming container');
  }
}

export async function removeContainer(
  endpointId: EnvironmentId,
  containerId: string,
  {
    nodeName,
    removeVolumes,
  }: { removeVolumes?: boolean; nodeName?: string } = {}
) {
  try {
    const { data } = await axios.delete<null | { message: string }>(
      buildDockerProxyUrl(endpointId, 'containers', containerId),
      {
        params: { v: removeVolumes ? 1 : 0, force: true },
        headers: { ...withAgentTargetHeader(nodeName) },
      }
    );

    if (data && data.message) {
      throw new PortainerError(data.message);
    }
  } catch (e) {
    throw parseAxiosError(e, 'Unable to remove container');
  }
}

export async function getContainerLogs(
  environmentId: EnvironmentId,
  containerId: ContainerId,
  params?: ContainerLogsParams
): Promise<string> {
  try {
    const { data } = await axios.get<string>(
      buildDockerProxyUrl(environmentId, 'containers', containerId, 'logs'),
      {
        params: _.pickBy(params),
      }
    );

    return data;
  } catch (e) {
    throw parseAxiosError(e, 'Unable to get container logs');
  }
}

export type StreamLogsParams = ContainerLogsParams & {
  /** follow=1 — keep the connection open and stream new lines as they arrive */
  follow?: boolean;
};

/**
 * Live-tail a container's logs over HTTP.
 *
 * Unlike `getContainerLogs` (axios, buffers the whole body), this uses `fetch`
 * so we can read the response body as a `ReadableStream` and surface the raw
 * byte chunks to the caller as they arrive (`follow=1`). Bytes are handed over
 * undecoded so the caller can demux Docker's binary multiplexed frames at the
 * byte level (UTF-8-decoding the whole stream would corrupt frame headers). The
 * backend already
 * streams `follow=1` transparently through the Docker proxy — including for
 * Agent/Edge environments — so no backend change is needed.
 *
 * Auth: the API uses an httpOnly JWT cookie (SameSite=Strict), so a same-origin
 * `fetch` with `credentials: 'include'` carries it automatically — no
 * `Authorization` header. CSRF middleware only guards mutating methods; logs is
 * a GET, so it is unaffected. The agent-target / manager-operation headers
 * (normally added by the axios `agentInterceptor`) are replicated here so the
 * stream also resolves the correct node on Agent/Edge environments.
 *
 * The caller drives lifetime via `signal`: aborting it (unmount, container
 * switch, pausing Live) cancels the in-flight fetch and ends the read loop.
 */
export async function streamContainerLogs(
  environmentId: EnvironmentId,
  containerId: ContainerId,
  params: StreamLogsParams,
  onChunk: (bytes: Uint8Array) => void,
  signal: AbortSignal
): Promise<void> {
  const path = buildDockerProxyUrl(
    environmentId,
    'containers',
    containerId,
    'logs'
  );

  // The fetch path bypasses the axios request interceptors, so apply the same
  // Docker max-API-version pinning axios applies to getContainerLogs. Reuse the
  // shared interceptor (it only reads/rewrites `config.url`).
  const pinnedConfig = await dockerMaxAPIVersionInterceptor({
    url: path,
  } as InternalAxiosRequestConfig);
  const effectivePath = pinnedConfig.url ?? path;

  const query = new URLSearchParams();
  // _.pickBy drops undefined/0/'' the same way the axios path does
  Object.entries(_.pickBy(params)).forEach(([key, value]) => {
    query.set(key, String(typeof value === 'boolean' ? Number(value) : value));
  });

  // axios baseURL is 'api'; mirror it here since fetch doesn't share it.
  const url = `api${effectivePath}?${query.toString()}`;

  const headers: Record<string, string> = {};
  const target = portainerAgentTargetHeader();
  if (target) {
    headers[agentTargetHeader] = target;
  }
  if (portainerAgentManagerOperation()) {
    headers['X-PortainerAgent-ManagerOperation'] = '1';
  }

  const response = await fetch(url, {
    signal,
    credentials: 'include',
    headers,
  });

  if (!response.ok) {
    throw new PortainerError(
      `Unable to stream container logs (HTTP ${response.status})`
    );
  }

  if (!response.body) {
    return;
  }

  const reader = response.body.getReader();

  try {
    for (;;) {

      const { value, done } = await reader.read();
      if (done) {
        break;
      }
      if (value) {
        // Hand over the raw bytes undecoded; the caller demuxes Docker's binary
        // frames and decodes complete lines itself. UTF-8 chars split across
        // chunks are reassembled there before decoding, so no decoder flush is
        // needed here. The caller flushes its own trailing partial line on end.
        onChunk(value);
      }
    }
  } finally {
    reader.releaseLock();
  }
}
