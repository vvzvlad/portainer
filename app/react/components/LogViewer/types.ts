import { StreamLogsParams } from '@/react/docker/containers/containers.service';

/**
 * Opens a (possibly following) log stream for a single resource and hands the
 * caller the raw byte chunks as they arrive. This is exactly the shape of
 * `streamContainerLogs` once its `environmentId`/`containerId` are bound, so a
 * container view passes `streamContainerLogs.bind(null, envId, containerId)` and
 * a future service/task view can pass its own equivalent without the viewer
 * knowing anything about Docker proxy URLs.
 *
 * The caller drives lifetime via `signal`: aborting it cancels the in-flight
 * request and ends the read loop.
 */
export type StreamLogsFn = (
  params: StreamLogsParams,
  onChunk: (bytes: Uint8Array) => void,
  signal: AbortSignal
) => Promise<void>;
