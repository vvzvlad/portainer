import { useCurrentStateAndParams } from '@uirouter/react';

import { AccessControlPanel } from '@/react/portainer/access-control/AccessControlPanel';
import { ContainerDetailsViewModel } from '@/docker/models/containerDetails';
import { ResourceControlType } from '@/react/portainer/access-control/types';
import { trimContainerName } from '@/docker/filters/utils';
import { useEnvironmentId } from '@/react/hooks/useEnvironmentId';
import { useEnvironmentRegistries } from '@/react/portainer/environments/queries/useEnvironmentRegistries';
import { Registry } from '@/react/portainer/registries/types/registry';

import { PageHeader } from '@@/PageHeader';
import { Crumb } from '@@/PageHeader/Breadcrumbs/Breadcrumbs';
import { findBestMatchRegistry } from '@@/ImageConfigFieldset/findRegistryMatch';

import { useContainer } from '../queries/useContainer';

import { ContainerActionsSection } from './ContainerActionsSection/ContainerActionsSection';
import { ContainerStatusSection } from './ContainerStatusSection/ContainerStatusSection';
import { CreateImageSection } from './CreateImageSection/CreateImageSection';
import { ContainerDetailsSection } from './ContainerDetailsSection/ContainerDetailsSection';
import { VolumesSection } from './VolumesSection/VolumesSection';
import { ContainerNetworksDatatable } from './ContainerNetworksDatatable';
import { HealthStatus } from './HealthStatus';

// ui-router state name for a container opened from within a stack. Kept as a
// single source of truth so a future state rename stays in sync with the
// breadcrumb logic (and its test) instead of silently falling back to the
// default crumb.
export const STACK_CONTAINER_STATE_NAME = 'docker.stacks.stack.container';

// Shape of the params handed to the docker.stacks.stack route when building the
// back-to-stack breadcrumb link. Values are strings (inherited route params)
// except `external`, which is emitted as a boolean and serialized to
// `external=true` by ui-router.
type StackLinkParams = {
  name?: string;
  stackId?: string;
  type?: string;
  regular?: string;
  orphaned?: string;
  orphanedRunning?: string;
  external?: boolean;
  tab?: string;
};

export function ItemView() {
  const environmentId = useEnvironmentId();
  const { state, params } = useCurrentStateAndParams();
  const { id: containerId, nodeName } = params;

  const containerQuery = useContainer(
    { environmentId, containerId, nodeName },
    { select: (c) => new ContainerDetailsViewModel(c) }
  );

  const registriesQuery = useEnvironmentRegistries(environmentId);

  if (
    containerQuery.isLoading ||
    !containerQuery.data ||
    !registriesQuery.data
  ) {
    return null;
  }

  const container = containerQuery.data;
  const registryId = getRegistryId(container, registriesQuery.data);

  async function handleSuccess() {
    containerQuery.refetch();
  }

  const containerName =
    trimContainerName(container.Name) || container.Id || 'unknown';

  return (
    <>
      <PageHeader
        title="Container details"
        breadcrumbs={getContainerBreadcrumbs(
          state?.name,
          params,
          containerName
        )}
      />

      <div className="mx-4 mb-4 space-y-4 [&>*]:block">
        <ContainerActionsSection
          environmentId={environmentId}
          nodeName={nodeName}
          container={container}
        />

        <ContainerStatusSection
          environmentId={environmentId}
          nodeName={nodeName}
          container={container}
          registryId={registryId}
        />
      </div>

      <AccessControlPanel
        resourceId={container.Id || ''}
        resourceControl={container.ResourceControl}
        resourceType={ResourceControlType.Container}
        onUpdateSuccess={handleSuccess}
        environmentId={environmentId}
      />

      {container.State?.Health && (
        <HealthStatus health={container.State.Health} />
      )}

      <div className="mx-4 mb-4 space-y-4 [&>*]:block">
        <CreateImageSection
          environmentId={environmentId}
          containerId={container.Id || ''}
        />

        <ContainerDetailsSection
          environmentId={environmentId}
          container={container}
          nodeName={nodeName}
        />

        <VolumesSection volumes={container.Mounts} nodeName={nodeName} />
      </div>

      {container.NetworkSettings?.Networks && (
        <ContainerNetworksDatatable
          dataset={container.NetworkSettings.Networks}
          containerId={containerId}
          nodeName={nodeName}
        />
      )}
    </>
  );
}

// When a container is opened from a stack (state STACK_CONTAINER_STATE_NAME),
// keep the stack trail in the breadcrumbs so the user can navigate back to the
// stack. Otherwise fall back to the global containers list.
function getContainerBreadcrumbs(
  stateName: string | undefined,
  params: Record<string, string | undefined>,
  containerName: string
): Array<Crumb | string> {
  if (stateName === STACK_CONTAINER_STATE_NAME) {
    return [
      { label: 'Stacks', link: 'docker.stacks' },
      {
        label: params.name || '',
        link: 'docker.stacks.stack',
        linkParams: buildStackLinkParams(params),
      },
      containerName,
    ];
  }

  return [{ label: 'Containers', link: 'docker.containers' }, containerName];
}

// Rebuild the params expected by the docker.stacks.stack route from the current
// (container) route params, which are inherited from the parent stack state.
function buildStackLinkParams(
  params: Record<string, string | undefined>
): StackLinkParams {
  // External stacks have no DB id; they are identified by name/type only.
  if (params.external === 'true') {
    return {
      name: params.name,
      type: params.type,
      external: true,
      tab: params.tab,
    };
  }

  return {
    name: params.name,
    stackId: params.stackId,
    type: params.type,
    regular: params.regular,
    orphaned: params.orphaned,
    orphanedRunning: params.orphanedRunning,
    tab: params.tab,
  };
}

function getRegistryId(
  container: ContainerDetailsViewModel,
  registries?: Array<Registry>
) {
  const imageName = container.Config?.Image;
  if (!imageName || !registries) {
    return undefined;
  }

  const registry = findBestMatchRegistry(imageName, registries);
  return registry?.Id;
}
