import { useCurrentStateAndParams } from '@uirouter/react';

import { useContainer } from '@/react/docker/containers/queries/useContainer';
import {
  streamContainerLogs,
  StreamLogsParams,
} from '@/react/docker/containers/containers.service';
import { ContainerDetailsViewModel } from '@/docker/models/containerDetails';
import { trimContainerName } from '@/docker/filters/utils';

import { InformationPanel } from '@@/InformationPanel';
import { TextTip } from '@@/Tip/TextTip';
import { Link } from '@@/Link';
import { LogViewer } from '@@/LogViewer';

export function LogView() {
  const {
    params: { endpointId: environmentId, id: containerId, nodeName },
  } = useCurrentStateAndParams();

  const containerQuery = useContainer(
    { environmentId, containerId, nodeName },
    {
      select: (c) => new ContainerDetailsViewModel(c),
    }
  );
  if (!containerQuery.data || containerQuery.isLoading) {
    return null;
  }

  const container = containerQuery.data;

  const logsEnabled =
    container.HostConfig?.LogConfig?.Type && // if a portion of the object path doesn't exist, logging is likely disabled
    container.HostConfig.LogConfig.Type !== 'none'; // if type === none logging is disabled

  if (!logsEnabled) {
    return <LogsDisabledInfoPanel />;
  }

  // A TTY container emits a single raw stream; a non-TTY container multiplexes
  // stdout/stderr with 8-byte frame headers the stream processor must demux.
  const hasFrameHeaders = !container.Config?.Tty;
  const resourceName = trimContainerName(container.Name || 'container');

  // Bind the environment/container so the viewer only knows the generic
  // "open a log stream" contract (StreamLogsFn), not Docker proxy specifics.
  function streamLogs(
    params: StreamLogsParams,
    onChunk: (bytes: Uint8Array) => void,
    signal: AbortSignal
  ) {
    return streamContainerLogs(
      environmentId,
      containerId,
      params,
      onChunk,
      signal
    );
  }

  return (
    <LogViewer
      streamLogs={streamLogs}
      resourceId={containerId}
      hasFrameHeaders={hasFrameHeaders}
      resourceName={resourceName}
    />
  );
}

function LogsDisabledInfoPanel() {
  const {
    params: { id: containerId, nodeName },
  } = useCurrentStateAndParams();

  return (
    <div className="row">
      <div className="col-sm-12">
        <InformationPanel>
          <TextTip color="blue">
            Logging is disabled for this container. If you want to re-enable
            logging, please{' '}
            <Link
              to="docker.containers.new"
              params={{ from: containerId, nodeName }}
              data-cy="redeploy-container-link"
            >
              redeploy your container
            </Link>{' '}
            and select a logging driver in the &quot;Command & logging&quot;
            panel.
          </TextTip>
        </InformationPanel>
      </div>
    </div>
  );
}
