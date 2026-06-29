import { Authorized } from '@/react/hooks/useUser';
import { EnvironmentId } from '@/react/portainer/environments/types';

import { ButtonGroup } from '@@/buttons';

import { ContainerId } from '../../types';

import { RecreateButton } from './SecondaryActions/RecreateButton';
import { UpdateNowButton } from './SecondaryActions/UpdateNowButton';
import { DuplicateEditButton } from './SecondaryActions/DuplicateEditButton';
import { useCanRecreateContainer } from './SecondaryActions/useCanRecreateContainer';
import { useCanDuplicateEditContainer } from './SecondaryActions/useCanDuplicateEditContainer';

interface Props {
  environmentId: EnvironmentId;
  containerId: ContainerId;
  nodeName?: string;
  containerImage: string;
  containerName: string;
  containerLabels?: Record<string, string>;
  containerAutoRemove: boolean | undefined;
  isPortainer: boolean;
  partOfSwarmService: boolean;
}

export function SecondaryActions({
  environmentId,
  containerId,
  nodeName,
  containerImage,
  containerName,
  containerLabels,
  containerAutoRemove = false,
  isPortainer,
  partOfSwarmService,
}: Props) {
  const displayRecreateButton = useCanRecreateContainer({
    autoRemove: containerAutoRemove,
    partOfSwarmService,
  });

  const displayDuplicateEditButton = useCanDuplicateEditContainer({
    autoRemove: containerAutoRemove,
    partOfSwarmService,
  });

  return (
    <Authorized authorizations="DockerContainerCreate">
      <ButtonGroup>
        {/* Self-hides unless the image is outdated; shown for stack & standalone. */}
        <UpdateNowButton
          environmentId={environmentId}
          containerId={containerId}
          nodeName={nodeName}
          containerImage={containerImage}
          containerName={containerName}
          labels={containerLabels}
          isPortainer={isPortainer}
        />

        {displayRecreateButton && (
          <RecreateButton
            environmentId={environmentId}
            containerId={containerId}
            nodeName={nodeName}
            containerImage={containerImage}
            isPortainer={isPortainer}
          />
        )}

        {displayDuplicateEditButton && (
          <DuplicateEditButton
            containerId={containerId}
            nodeName={nodeName}
            isPortainer={isPortainer}
          />
        )}
      </ButtonGroup>
    </Authorized>
  );
}
