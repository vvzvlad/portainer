import { render, screen } from '@testing-library/react';

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
});
