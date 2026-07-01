import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { vi } from 'vitest';

import { COMPOSE_STACK_NAME_LABEL } from '@/react/constants';
import { StackType } from '@/react/common/stacks/types';
import { ContainerImageStatusValue } from '@/react/docker/containers/queries/useContainerImageStatus';

import { UpdateNowButton } from './UpdateNowButton';

const mockUseImageStatus = vi.fn();
const mockUseStacks = vi.fn();
const mockUseAuthorizations = vi.fn();
const mockMutate = vi.fn();
const mockConfirmContainerRecreation = vi.fn();
const mockConfirmStackUpdate = vi.fn();

vi.mock('@uirouter/react', () => ({
  useRouter: () => ({ stateService: { go: vi.fn() } }),
}));

// The two confirm dialogs are the async gate before the mutation dispatches;
// stub them so a test can drive the resolved (confirmed) branch.
vi.mock('@/react/docker/containers/ItemView/ConfirmRecreationModal', () => ({
  confirmContainerRecreation: (...args: unknown[]) =>
    mockConfirmContainerRecreation(...args),
}));

vi.mock('@/react/common/stacks/common/confirm-stack-update', () => ({
  confirmStackUpdate: (...args: unknown[]) => mockConfirmStackUpdate(...args),
}));

vi.mock('@/portainer/services/notifications', () => ({
  notifySuccess: vi.fn(),
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

// Mock the concrete mutation module so both the `update` index re-export and the
// shared `useApplyContainerImageUpdate` hook (which imports it directly) resolve
// to the stub.
vi.mock('@/react/docker/containers/update/useUpdateContainerImage', () => ({
  useUpdateContainerImage: () => ({ mutate: mockMutate, isLoading: false }),
  invalidateContainerUpdateQueries: vi.fn(),
}));

function setStatus(status?: ContainerImageStatusValue) {
  mockUseImageStatus.mockReturnValue({
    data: status ? { Status: status } : undefined,
  });
}

function renderButton(
  props: Partial<React.ComponentProps<typeof UpdateNowButton>> = {}
) {
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

  // apply() dispatch: clicking confirms, then routes to the mutation exactly once
  // with the resolved pullImage, via the standalone (recreate) vs stack (redeploy)
  // confirm dialog.
  it('applies a standalone update via the recreate confirm, mutating once with pullImage', async () => {
    setStatus('outdated');
    mockConfirmContainerRecreation.mockResolvedValue({ pullLatest: true });

    renderButton(); // no compose label -> standalone

    fireEvent.click(screen.getByTestId('update-now-button'));

    await waitFor(() => expect(mockMutate).toHaveBeenCalledTimes(1));
    expect(mockConfirmContainerRecreation).toHaveBeenCalledTimes(1);
    expect(mockConfirmStackUpdate).not.toHaveBeenCalled();
    expect(mockMutate).toHaveBeenCalledWith(
      expect.objectContaining({ pullImage: true }),
      expect.anything()
    );
  });

  it('applies a stack update via the stack confirm, mutating once with pullImage', async () => {
    setStatus('outdated');
    mockUseStacks.mockReturnValue({ data: [composeStack], isLoading: false });
    mockUseAuthorizations.mockReturnValue({ authorized: true });
    mockConfirmStackUpdate.mockResolvedValue({ repullImageAndRedeploy: false });

    renderButton({
      environmentId: 3,
      labels: { [COMPOSE_STACK_NAME_LABEL]: 'my-stack' },
    });

    fireEvent.click(screen.getByTestId('update-now-button'));

    await waitFor(() => expect(mockMutate).toHaveBeenCalledTimes(1));
    expect(mockConfirmStackUpdate).toHaveBeenCalledTimes(1);
    expect(mockConfirmContainerRecreation).not.toHaveBeenCalled();
    expect(mockMutate).toHaveBeenCalledWith(
      expect.objectContaining({ pullImage: false }),
      expect.anything()
    );
  });

  it('does not mutate when the confirm dialog is dismissed', async () => {
    setStatus('outdated');
    mockConfirmContainerRecreation.mockResolvedValue(false);

    renderButton();

    fireEvent.click(screen.getByTestId('update-now-button'));

    await waitFor(() =>
      expect(mockConfirmContainerRecreation).toHaveBeenCalledTimes(1)
    );
    expect(mockMutate).not.toHaveBeenCalled();
  });
});
