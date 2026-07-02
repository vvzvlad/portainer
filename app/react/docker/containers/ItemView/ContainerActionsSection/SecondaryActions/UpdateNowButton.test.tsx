import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { vi } from 'vitest';

import { COMPOSE_STACK_NAME_LABEL } from '@/react/constants';
import { ContainerImageStatusValue } from '@/react/docker/containers/queries/useContainerImageStatus';

import { UpdateNowButton } from './UpdateNowButton';

const mockUseImageStatus = vi.fn();
const mockMutate = vi.fn();
const mockConfirmContainerRecreation = vi.fn();

vi.mock('@uirouter/react', () => ({
  useRouter: () => ({ stateService: { go: vi.fn() } }),
}));

// The recreate confirm dialog is the async gate before the mutation dispatches;
// stub it so a test can drive the resolved (confirmed) branch.
vi.mock('@/react/docker/containers/ItemView/ConfirmRecreationModal', () => ({
  confirmContainerRecreation: (...args: unknown[]) =>
    mockConfirmContainerRecreation(...args),
}));

vi.mock('@/portainer/services/notifications', () => ({
  notifySuccess: vi.fn(),
}));

vi.mock('@/react/docker/containers/queries/useContainerImageStatus', () => ({
  useContainerImageStatus: () => mockUseImageStatus(),
}));

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

  it('disables the action for Portainer\'s own container', () => {
    setStatus('outdated');
    renderButton({ isPortainer: true });
    expect(screen.getByTestId('update-now-button')).toBeDisabled();
  });

  it('enables a compose-managed container (recreate keeps it in its project)', () => {
    setStatus('outdated');
    renderButton({
      labels: { [COMPOSE_STACK_NAME_LABEL]: 'my-stack' },
    });
    expect(screen.getByTestId('update-now-button')).toBeEnabled();
  });

  // apply() dispatch: clicking confirms, then routes to the single-container
  // recreate mutation exactly once with the resolved pullImage.
  it('applies a single-container recreate via the confirm, mutating once with pullImage', async () => {
    setStatus('outdated');
    mockConfirmContainerRecreation.mockResolvedValue({ pullLatest: true });

    renderButton();

    fireEvent.click(screen.getByTestId('update-now-button'));

    await waitFor(() => expect(mockMutate).toHaveBeenCalledTimes(1));
    expect(mockConfirmContainerRecreation).toHaveBeenCalledTimes(1);
    expect(mockMutate).toHaveBeenCalledWith(
      expect.objectContaining({ pullImage: true }),
      expect.anything()
    );
  });

  it('recreates a compose-managed container the same way (never redeploys a stack)', async () => {
    setStatus('outdated');
    mockConfirmContainerRecreation.mockResolvedValue({ pullLatest: false });

    renderButton({
      labels: { [COMPOSE_STACK_NAME_LABEL]: 'my-stack' },
    });

    fireEvent.click(screen.getByTestId('update-now-button'));

    await waitFor(() => expect(mockMutate).toHaveBeenCalledTimes(1));
    expect(mockConfirmContainerRecreation).toHaveBeenCalledTimes(1);
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
