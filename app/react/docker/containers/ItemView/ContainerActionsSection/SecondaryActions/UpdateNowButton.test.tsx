import { render, screen } from '@testing-library/react';
import { vi } from 'vitest';

import { COMPOSE_STACK_NAME_LABEL } from '@/react/constants';
import { StackType } from '@/react/common/stacks/types';
import { ContainerImageStatusValue } from '@/react/docker/containers/queries/useContainerImageStatus';

import { UpdateNowButton } from './UpdateNowButton';

const mockUseImageStatus = vi.fn();
const mockUseStacks = vi.fn();
const mockUseAuthorizations = vi.fn();
const mockMutate = vi.fn();

vi.mock('@uirouter/react', () => ({
  useRouter: () => ({ stateService: { go: vi.fn() } }),
}));

vi.mock('@/react/docker/containers/queries/useContainerImageStatus', () => ({
  useContainerImageStatus: () => mockUseImageStatus(),
}));

vi.mock('@/react/common/stacks/queries/useStacks', () => ({
  useStacks: () => mockUseStacks(),
}));

vi.mock('@/react/hooks/useUser', () => ({
  useAuthorizations: () => mockUseAuthorizations(),
}));

const composeStack = {
  Id: 7,
  Name: 'my-stack',
  EndpointId: 3,
  Type: StackType.DockerCompose,
};

vi.mock(
  '@/react/docker/containers/update',
  async (importOriginal: () => Promise<object>) => ({
    ...(await importOriginal()),
    useUpdateContainerImage: () => ({ mutate: mockMutate, isLoading: false }),
  })
);

function setStatus(status?: ContainerImageStatusValue) {
  mockUseImageStatus.mockReturnValue({ data: status ? { Status: status } : undefined });
}

function renderButton(props: Partial<React.ComponentProps<typeof UpdateNowButton>> = {}) {
  return render(
    <UpdateNowButton
      environmentId={3}
      containerId="abc"
      containerImage="nginx:latest"
      containerName="web"
      isPortainer={false}
      {...props}
    />
  );
}

describe('UpdateNowButton', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockUseStacks.mockReturnValue({ data: [], isLoading: false });
    mockUseAuthorizations.mockReturnValue({ authorized: true });
  });

  it('renders when the image is outdated', () => {
    setStatus('outdated');
    renderButton();
    expect(screen.getByTestId('update-now-button')).toBeInTheDocument();
  });

  it('is hidden when the image is up to date', () => {
    setStatus('updated');
    renderButton();
    expect(screen.queryByTestId('update-now-button')).not.toBeInTheDocument();
  });

  it('is hidden when the status is unknown', () => {
    setStatus(undefined);
    renderButton();
    expect(screen.queryByTestId('update-now-button')).not.toBeInTheDocument();
  });

  it('disables the action for externally-managed compose containers', () => {
    setStatus('outdated');
    renderButton({
      labels: { [COMPOSE_STACK_NAME_LABEL]: 'not-in-portainer' },
    });
    expect(screen.getByTestId('update-now-button')).toBeDisabled();
  });

  it('enables a stack container when the user can update stacks', () => {
    setStatus('outdated');
    mockUseStacks.mockReturnValue({ data: [composeStack], isLoading: false });
    mockUseAuthorizations.mockReturnValue({ authorized: true });
    renderButton({
      environmentId: 3,
      labels: { [COMPOSE_STACK_NAME_LABEL]: 'my-stack' },
    });
    expect(screen.getByTestId('update-now-button')).toBeEnabled();
  });

  it('disables a stack container when the user lacks PortainerStackUpdate', () => {
    setStatus('outdated');
    mockUseStacks.mockReturnValue({ data: [composeStack], isLoading: false });
    mockUseAuthorizations.mockReturnValue({ authorized: false });
    renderButton({
      environmentId: 3,
      labels: { [COMPOSE_STACK_NAME_LABEL]: 'my-stack' },
    });
    expect(screen.getByTestId('update-now-button')).toBeDisabled();
  });
});
