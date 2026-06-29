import { render, screen } from '@testing-library/react';
import { vi } from 'vitest';

import { withTestRouter } from '@/react/test-utils/withRouter';
import { withTestQueryProvider } from '@/react/test-utils/withTestQuery';

import { AccessDatatable } from './AccessDatatable';
import { Access } from './types';

function createMockAccess(overrides: Partial<Access> = {}): Access {
  return {
    Id: 1,
    Name: 'Test User',
    Type: 'user',
    Role: { Id: 1, Name: 'Admin' },
    ...overrides,
  } as Access;
}

function renderComponent(
  props: Partial<Parameters<typeof AccessDatatable>[0]> = {}
) {
  const defaultProps = {
    tableKey: 'test-access-table',
    dataset: [],
    onRemove: vi.fn(),
    isLoading: false,
  };

  const Wrapped = withTestQueryProvider(withTestRouter(AccessDatatable));
  return render(<Wrapped {...defaultProps} {...props} />);
}

describe('AccessDatatable', () => {
  describe('loading state', () => {
    it('should show loading state when isLoading is true', () => {
      renderComponent({
        isLoading: true,
        dataset: [],
      });

      expect(screen.getByLabelText('Access')).toBeVisible();
      // Datatable shows loading indicator when isLoading is true
      expect(screen.getByText('Loading...')).toBeVisible();
    });

    it('should not show loading state when isLoading is false', () => {
      renderComponent({
        isLoading: false,
        dataset: [createMockAccess()],
      });

      expect(screen.queryByText('Loading...')).not.toBeInTheDocument();
    });
  });

  describe('content display', () => {
    it('should display access data in the table', () => {
      const mockAccess = createMockAccess({ Name: 'John Doe' });
      renderComponent({
        isLoading: false,
        dataset: [mockAccess],
      });

      expect(screen.getByText('John Doe')).toBeVisible();
    });

    it('should display empty state when dataset is empty and not loading', () => {
      renderComponent({
        isLoading: false,
        dataset: [],
      });

      expect(screen.getByText('No items available.')).toBeVisible();
    });

    it('should render the datatable title', () => {
      renderComponent({
        isLoading: false,
        dataset: [],
      });

      expect(screen.getByText('Access')).toBeVisible();
    });
  });

  describe('inherited access', () => {
    it('should show inherited access warning when inheritFrom is true', () => {
      renderComponent({
        isLoading: false,
        dataset: [],
        inheritFrom: true,
      });

      // Multiple "Access tagged as" divs are rendered (one for inherited, one for override)
      const elements = screen.getAllByText(/Access tagged as/);
      expect(elements.length).toBeGreaterThan(0);
      expect(elements[0]).toBeVisible();
    });
  });
});
