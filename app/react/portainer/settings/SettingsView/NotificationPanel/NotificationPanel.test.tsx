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
  it('renders the webhook URL read from the API', async () => {
    server.use(
      http.get('/api/settings', () =>
        HttpResponse.json({
          ContainerAutomation: {
            Notification: {
              WebhookURL: 'https://example.com/notify?msg={{message}}',
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
      expect(screen.getByLabelText(/Webhook URL/i)).toHaveValue(
        'https://example.com/notify?msg={{message}}'
      );
    });
  });

  it('disables the save button on an invalid URL', async () => {
    const user = userEvent.setup();

    server.use(
      http.get('/api/settings', () =>
        HttpResponse.json({
          ContainerAutomation: { Notification: { WebhookURL: '' } },
        })
      )
    );

    renderComponent();

    const input = await screen.findByLabelText(/Webhook URL/i);
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

  it('includes the webhook URL in the saved payload', async () => {
    const user = userEvent.setup();

    let savedPayload: unknown;
    server.use(
      http.get('/api/settings', () =>
        HttpResponse.json({
          ContainerAutomation: { Notification: { WebhookURL: '' } },
        })
      ),
      http.put('/api/settings', async ({ request }) => {
        savedPayload = await request.json();
        return HttpResponse.json({});
      })
    );

    renderComponent();

    const input = await screen.findByLabelText(/Webhook URL/i);
    await user.type(input, 'https://hook.example/notify');

    const save = screen.getByRole('button', {
      name: /Save notification settings/i,
    });
    await waitFor(() => expect(save).toBeEnabled());
    await user.click(save);

    await waitFor(() => {
      expect(savedPayload).toEqual({
        ContainerAutomation: {
          Notification: {
            WebhookURL: 'https://hook.example/notify',
          },
        },
      });
    });
  });
});
