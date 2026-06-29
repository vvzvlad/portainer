import { render, screen, waitFor } from '@testing-library/react';
import { HttpResponse, http } from 'msw';

import { withTestRouter } from '@/react/test-utils/withRouter';
import { withTestQueryProvider } from '@/react/test-utils/withTestQuery';
import { withUserProvider } from '@/react/test-utils/withUserProvider';
import { server } from '@/setup-tests/server';

import { AutoHealPanel } from './AutoHealPanel';

function renderComponent() {
  const Wrapped = withTestQueryProvider(
    withUserProvider(withTestRouter(AutoHealPanel))
  );
  return render(<Wrapped />);
}

describe('AutoHealPanel', () => {
  it('renders the auto-heal settings read from the API', async () => {
    server.use(
      http.get('/api/settings', () =>
        HttpResponse.json({
          ContainerAutomation: {
            AutoHeal: {
              Enabled: true,
              CheckInterval: '45s',
              Scope: 'all',
            },
          },
        })
      )
    );

    renderComponent();

    expect(await screen.findByText('Container auto-heal')).toBeInTheDocument();

    await waitFor(() => {
      const interval = screen.getByLabelText(/Check interval/i);
      expect(interval).toHaveValue('45s');
    });

    const scope = screen.getByLabelText(/Scope/i);
    expect(scope).toHaveValue('all');

    expect(
      screen.getByRole('checkbox', { name: /Enable auto-heal/i })
    ).toBeChecked();
  });

  it('defaults the interval and scope when missing', async () => {
    server.use(
      http.get('/api/settings', () =>
        HttpResponse.json({
          ContainerAutomation: {
            AutoHeal: {
              Enabled: false,
              CheckInterval: '',
              Scope: 'labeled',
            },
          },
        })
      )
    );

    renderComponent();

    await waitFor(() => {
      const interval = screen.getByLabelText(/Check interval/i);
      expect(interval).toHaveValue('30s');
    });

    expect(
      screen.getByRole('checkbox', { name: /Enable auto-heal/i })
    ).not.toBeChecked();
  });
});
