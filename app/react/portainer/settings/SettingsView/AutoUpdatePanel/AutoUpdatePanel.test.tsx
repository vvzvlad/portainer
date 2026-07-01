import { render, screen, waitFor } from '@testing-library/react';
import { HttpResponse, http } from 'msw';

import { withTestRouter } from '@/react/test-utils/withRouter';
import { withTestQueryProvider } from '@/react/test-utils/withTestQuery';
import { withUserProvider } from '@/react/test-utils/withUserProvider';
import { server } from '@/setup-tests/server';

import { AutoUpdatePanel } from './AutoUpdatePanel';

function renderComponent() {
  const Wrapped = withTestQueryProvider(
    withUserProvider(withTestRouter(AutoUpdatePanel))
  );
  return render(<Wrapped />);
}

describe('AutoUpdatePanel', () => {
  it('renders the auto-update settings read from the API', async () => {
    server.use(
      http.get('/api/settings', () =>
        HttpResponse.json({
          ContainerAutomation: {
            AutoHeal: {
              Enabled: false,
              CheckInterval: '30s',
              Scope: 'labeled',
            },
            AutoUpdate: {
              Enabled: true,
              PollInterval: '12h',
              Scope: 'all',
              Cleanup: true,
              RollbackOnFailure: true,
              RollbackTimeout: '90s',
            },
          },
        })
      )
    );

    renderComponent();

    expect(await screen.findByText('Container auto-update')).toBeInTheDocument();

    await waitFor(() => {
      const interval = screen.getByLabelText(/Poll interval/i);
      expect(interval).toHaveValue('12h');
    });

    const scope = screen.getByLabelText(/Scope/i);
    expect(scope).toHaveValue('all');

    expect(
      screen.getByRole('checkbox', { name: /Enable auto-update/i })
    ).toBeChecked();

    expect(
      screen.getByRole('checkbox', {
        name: /Remove dangling old images after update/i,
      })
    ).toBeChecked();

    expect(
      screen.getByRole('checkbox', {
        name: /Roll back on failed health check/i,
      })
    ).toBeChecked();

    expect(screen.getByLabelText(/Rollback timeout/i)).toHaveValue('90s');
  });

  it('defaults the poll interval when missing', async () => {
    server.use(
      http.get('/api/settings', () =>
        HttpResponse.json({
          ContainerAutomation: {
            AutoHeal: {
              Enabled: false,
              CheckInterval: '30s',
              Scope: 'labeled',
            },
            AutoUpdate: {
              Enabled: false,
              PollInterval: '',
              Scope: 'labeled',
              Cleanup: false,
              RollbackOnFailure: false,
              RollbackTimeout: '',
            },
          },
        })
      )
    );

    renderComponent();

    await waitFor(() => {
      const interval = screen.getByLabelText(/Poll interval/i);
      expect(interval).toHaveValue('6h');
    });

    expect(
      screen.getByRole('checkbox', { name: /Enable auto-update/i })
    ).not.toBeChecked();

    expect(
      screen.getByRole('checkbox', {
        name: /Roll back on failed health check/i,
      })
    ).not.toBeChecked();

    // Falls back to the default rollback timeout when missing.
    expect(screen.getByLabelText(/Rollback timeout/i)).toHaveValue('120s');
  });
});
