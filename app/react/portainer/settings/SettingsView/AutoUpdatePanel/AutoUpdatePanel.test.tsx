import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';

import { withTestRouter } from '@/react/test-utils/withRouter';
import { withTestQueryProvider } from '@/react/test-utils/withTestQuery';
import { withUserProvider } from '@/react/test-utils/withUserProvider';
import { server } from '@/setup-tests/server';
import { containerAutomationWebhookUrl } from '@/portainer/helpers/webhookHelper';

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

  function seedSettings(token?: string, enabled = true) {
    server.use(
      http.get('/api/settings', () =>
        HttpResponse.json({
          ContainerAutomation: {
            AutoHeal: { Enabled: false, CheckInterval: '30s', Scope: 'labeled' },
            AutoUpdate: {
              Enabled: enabled,
              PollInterval: '6h',
              Scope: 'labeled',
              Cleanup: false,
              RollbackOnFailure: false,
              RollbackTimeout: '120s',
              WebhookToken: token,
            },
          },
        })
      )
    );
  }

  it('renders the trigger webhook URL and Regenerate/Clear when a token exists', async () => {
    const token = 'abc-123-token';
    seedSettings(token);

    renderComponent();

    const urlField = await screen.findByLabelText(/Update trigger webhook/i);
    // The URL is built via the shared helper (sub-path / base-href aware), not a
    // bare origin — assert against the same helper.
    await waitFor(() =>
      expect(urlField).toHaveValue(containerAutomationWebhookUrl(token))
    );

    expect(
      screen.getByRole('button', { name: /Regenerate/i })
    ).toBeInTheDocument();
    expect(screen.getByRole('button', { name: /Clear/i })).toBeInTheDocument();
  });

  it('warns that the trigger is inactive when a token exists but auto-update is disabled', async () => {
    seedSettings('abc-123-token', false);

    renderComponent();

    // The disabled-state note (a POST would 409) must be shown so the UI does not
    // contradict the server.
    expect(
      await screen.findByText(/trigger is inactive/i)
    ).toBeInTheDocument();
  });

  it('does NOT show the inactive note when auto-update is enabled', async () => {
    seedSettings('abc-123-token', true);

    renderComponent();

    // Wait for the section to render, then confirm the note is absent.
    await screen.findByLabelText(/Update trigger webhook/i);
    expect(screen.queryByText(/trigger is inactive/i)).not.toBeInTheDocument();
  });

  it('shows Generate and no Clear when no token exists', async () => {
    seedSettings(undefined);

    renderComponent();

    expect(
      await screen.findByRole('button', { name: /Generate/i })
    ).toBeInTheDocument();
    expect(
      screen.queryByRole('button', { name: /Clear/i })
    ).not.toBeInTheDocument();

    const urlField = screen.getByLabelText(/Update trigger webhook/i);
    expect(urlField).toHaveValue('');
  });

  it('requests a server-side token regeneration without sending a token value', async () => {
    seedSettings(undefined);

    let putBody: unknown;
    server.use(
      http.put('/api/settings', async ({ request }) => {
        putBody = await request.json();
        return HttpResponse.json({});
      })
    );

    renderComponent();

    const generate = await screen.findByRole('button', { name: /Generate/i });
    await userEvent.click(generate);

    await waitFor(() => expect(putBody).toBeDefined());
    expect(putBody).toEqual({
      ContainerAutomation: { AutoUpdate: { RegenerateWebhookToken: true } },
    });
  });

  it('requests clearing the token', async () => {
    seedSettings('some-token');

    let putBody: unknown;
    server.use(
      http.put('/api/settings', async ({ request }) => {
        putBody = await request.json();
        return HttpResponse.json({});
      })
    );

    renderComponent();

    const clear = await screen.findByRole('button', { name: /Clear/i });
    await userEvent.click(clear);

    await waitFor(() => expect(putBody).toBeDefined());
    expect(putBody).toEqual({
      ContainerAutomation: { AutoUpdate: { ClearWebhookToken: true } },
    });
  });
});
