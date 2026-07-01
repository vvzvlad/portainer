import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';

import { withTestRouter } from '@/react/test-utils/withRouter';
import { withTestQueryProvider } from '@/react/test-utils/withTestQuery';
import { withUserProvider } from '@/react/test-utils/withUserProvider';
import { server } from '@/setup-tests/server';

import { NotificationPanel } from './NotificationPanel';

function renderComponent() {
  const Wrapped = withTestQueryProvider(
    withUserProvider(withTestRouter(NotificationPanel))
  );
  return render(<Wrapped />);
}

describe('NotificationPanel', () => {
  it('renders both webhook URLs read from the API', async () => {
    server.use(
      http.get('/api/settings', () =>
        HttpResponse.json({
          ContainerAutomation: {
            Notification: {
              UpdateWebhookURL: 'https://example.com/update?msg={{message}}',
              HealWebhookURL: 'https://example.com/heal?msg={{message}}',
            },
          },
        })
      )
    );

    renderComponent();

    expect(
      await screen.findByText('Container automation notifications')
    ).toBeInTheDocument();

    await waitFor(() => {
      expect(screen.getByLabelText(/Auto-update webhook URL/i)).toHaveValue(
        'https://example.com/update?msg={{message}}'
      );
      expect(screen.getByLabelText(/Auto-heal webhook URL/i)).toHaveValue(
        'https://example.com/heal?msg={{message}}'
      );
    });
  });

  it('disables the save button on an invalid auto-update URL', async () => {
    const user = userEvent.setup();

    server.use(
      http.get('/api/settings', () =>
        HttpResponse.json({
          ContainerAutomation: {
            Notification: { UpdateWebhookURL: '', HealWebhookURL: '' },
          },
        })
      )
    );

    renderComponent();

    const input = await screen.findByLabelText(/Auto-update webhook URL/i);
    await user.type(input, 'not-a-url');

    await waitFor(() => {
      expect(
        screen.getByText('Must be a valid http(s) URL')
      ).toBeInTheDocument();
    });

    expect(
      screen.getByRole('button', { name: /Save notification settings/i })
    ).toBeDisabled();
  });

  it('disables the save button on an invalid auto-heal URL', async () => {
    const user = userEvent.setup();

    server.use(
      http.get('/api/settings', () =>
        HttpResponse.json({
          ContainerAutomation: {
            Notification: { UpdateWebhookURL: '', HealWebhookURL: '' },
          },
        })
      )
    );

    renderComponent();

    const input = await screen.findByLabelText(/Auto-heal webhook URL/i);
    await user.type(input, 'not-a-url');

    await waitFor(() => {
      expect(
        screen.getByText('Must be a valid http(s) URL')
      ).toBeInTheDocument();
    });

    expect(
      screen.getByRole('button', { name: /Save notification settings/i })
    ).toBeDisabled();
  });

  it('includes both webhook URLs in the saved payload', async () => {
    const user = userEvent.setup();

    let savedPayload: unknown;
    server.use(
      http.get('/api/settings', () =>
        HttpResponse.json({
          ContainerAutomation: {
            Notification: { UpdateWebhookURL: '', HealWebhookURL: '' },
          },
        })
      ),
      http.put('/api/settings', async ({ request }) => {
        savedPayload = await request.json();
        return HttpResponse.json({});
      })
    );

    renderComponent();

    const updateInput = await screen.findByLabelText(/Auto-update webhook URL/i);
    await user.type(updateInput, 'https://hook.example/update');

    const healInput = screen.getByLabelText(/Auto-heal webhook URL/i);
    await user.type(healInput, 'https://hook.example/heal');

    const save = screen.getByRole('button', {
      name: /Save notification settings/i,
    });
    await waitFor(() => expect(save).toBeEnabled());
    await user.click(save);

    await waitFor(() => {
      expect(savedPayload).toEqual({
        ContainerAutomation: {
          Notification: {
            UpdateWebhookURL: 'https://hook.example/update',
            HealWebhookURL: 'https://hook.example/heal',
          },
        },
      });
    });
  });
});
