import { render, screen, fireEvent } from '@testing-library/react';
import { vi } from 'vitest';

import { ContainerImageStatusValue } from '../../queries/useContainerImageStatus';

import { UpdateStatusBadge } from './UpdateStatusBadge';

describe('UpdateStatusBadge', () => {
  it('renders "Update available" when the image is outdated', () => {
    render(<UpdateStatusBadge status="outdated" />);

    expect(screen.getByText('Update available')).toBeInTheDocument();
  });

  it('renders an up-to-date indicator when the image is updated', () => {
    render(<UpdateStatusBadge status="updated" />);

    expect(screen.getByText('Up to date')).toBeInTheDocument();
  });

  it('shows a spinner while loading', () => {
    render(<UpdateStatusBadge isLoading />);

    expect(
      screen.getByLabelText('Checking for image updates')
    ).toBeInTheDocument();
  });

  it.each<ContainerImageStatusValue>([
    'skipped',
    'error',
    'processing',
    'preparing',
  ])('renders nothing for the neutral status "%s"', (status) => {
    const { container } = render(<UpdateStatusBadge status={status} />);

    expect(container).toBeEmptyDOMElement();
  });

  it('renders nothing when status is undefined and not loading', () => {
    const { container } = render(<UpdateStatusBadge />);

    expect(container).toBeEmptyDOMElement();
  });

  describe('interactive behavior', () => {
    it('renders "Update available" as a plain span when no handler is passed', () => {
      render(<UpdateStatusBadge status="outdated" />);

      expect(
        screen.queryByRole('button', { name: /update available/i })
      ).not.toBeInTheDocument();
    });

    it('renders "Update available" as a button and fires the handler on click', () => {
      const onUpdateClick = vi.fn();
      render(
        <UpdateStatusBadge status="outdated" onUpdateClick={onUpdateClick} />
      );

      const button = screen.getByRole('button', {
        name: /update available/i,
      });
      fireEvent.click(button);

      expect(onUpdateClick).toHaveBeenCalledTimes(1);
    });

    it('renders "Up to date" as a plain span when no handler is passed', () => {
      render(<UpdateStatusBadge status="updated" />);

      expect(
        screen.queryByRole('button', { name: /up to date/i })
      ).not.toBeInTheDocument();
    });

    it('renders "Up to date" as a button and fires the recheck handler on click', () => {
      const onRecheckClick = vi.fn();
      render(
        <UpdateStatusBadge status="updated" onRecheckClick={onRecheckClick} />
      );

      const button = screen.getByRole('button', { name: /up to date/i });
      fireEvent.click(button);

      expect(onRecheckClick).toHaveBeenCalledTimes(1);
    });

    it('shows a pending affordance and disables the update button while updating', () => {
      const onUpdateClick = vi.fn();
      render(
        <UpdateStatusBadge
          status="outdated"
          onUpdateClick={onUpdateClick}
          isUpdating
        />
      );

      const button = screen.getByRole('button', {
        name: /updating the image/i,
      });
      expect(button).toBeDisabled();
      expect(screen.getByText('Updating...')).toBeInTheDocument();

      fireEvent.click(button);
      expect(onUpdateClick).not.toHaveBeenCalled();
    });

    it('shows a spinner and disables the recheck button while rechecking', () => {
      const onRecheckClick = vi.fn();
      render(
        <UpdateStatusBadge
          status="updated"
          onRecheckClick={onRecheckClick}
          isRechecking
        />
      );

      const button = screen.getByRole('button', { name: /up to date/i });
      expect(button).toBeDisabled();

      fireEvent.click(button);
      expect(onRecheckClick).not.toHaveBeenCalled();
    });
  });
});
