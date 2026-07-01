import { Webhook } from 'lucide-react';
import { Field, Form, Formik, useFormikContext } from 'formik';

import { notifySuccess } from '@/portainer/services/notifications';

import { Widget } from '@@/Widget';
import { LoadingButton } from '@@/buttons';
import { FormControl } from '@@/form-components/FormControl';
import { Input } from '@@/form-components/Input';
import { TextTip } from '@@/Tip/TextTip';

import { useSettings, useUpdateSettingsMutation } from '../../queries';

import { Values } from './types';
import { validation } from './validation';

export function NotificationPanel() {
  const settingsQuery = useSettings((settings) => settings.ContainerAutomation);
  const mutation = useUpdateSettingsMutation();

  if (!settingsQuery.data) {
    return null;
  }

  const notification = settingsQuery.data.Notification;
  const initialValues: Values = {
    updateWebhookUrl: notification?.UpdateWebhookURL || '',
    healWebhookUrl: notification?.HealWebhookURL || '',
  };

  return (
    <Widget>
      <Widget.Title
        icon={Webhook}
        title="Container automation notifications"
      />
      <Widget.Body>
        <div className="mb-3">
          <TextTip color="blue">
            Portainer can call an HTTP URL for container-automation events so you
            can forward them to chat or a custom endpoint. The two webhooks are
            configured independently: the auto-update URL is called on image
            update, rollback and failed-update events, and the auto-heal URL on
            auto-heal restarts. Set only one to notify on that mechanism alone.
            For each URL, include the <code>{'{{message}}'}</code> placeholder to
            have the URL-encoded event message substituted into the address (the
            URL is then fetched with GET); when the placeholder is absent, the
            plain-text message is POSTed as the request body instead. The message
            looks like{' '}
            <code>Environment | prod / Container [nginx] / Auto-heal: ...</code>.
            Leave a URL empty to disable that mechanism. Delivery is best-effort
            and never blocks or delays an update or heal.
          </TextTip>
        </div>

        <Formik
          initialValues={initialValues}
          onSubmit={handleSubmit}
          validationSchema={validation}
          validateOnMount
          enableReinitialize
        >
          <InnerForm isLoading={mutation.isLoading} />
        </Formik>
      </Widget.Body>
    </Widget>
  );

  function handleSubmit(values: Values) {
    mutation.mutate(
      {
        ContainerAutomation: {
          Notification: {
            UpdateWebhookURL: values.updateWebhookUrl,
            HealWebhookURL: values.healWebhookUrl,
          },
        },
      },
      {
        onSuccess() {
          notifySuccess('Success', 'Notification settings updated');
        },
      }
    );
  }
}

function InnerForm({ isLoading }: { isLoading: boolean }) {
  const { isValid, errors } = useFormikContext<Values>();

  return (
    <Form className="form-horizontal">
      <FormControl
        label="Auto-update webhook URL"
        inputId="notification_update_webhook_url"
        errors={errors.updateWebhookUrl}
      >
        <Field
          as={Input}
          id="notification_update_webhook_url"
          placeholder="e.g. https://example.com/update?msg={{message}}"
          name="updateWebhookUrl"
          data-cy="settings-notificationUpdateWebhookUrl"
        />
      </FormControl>

      <FormControl
        label="Auto-heal webhook URL"
        inputId="notification_heal_webhook_url"
        errors={errors.healWebhookUrl}
      >
        <Field
          as={Input}
          id="notification_heal_webhook_url"
          placeholder="e.g. https://example.com/heal?msg={{message}}"
          name="healWebhookUrl"
          data-cy="settings-notificationHealWebhookUrl"
        />
      </FormControl>

      <div className="form-group">
        <div className="col-sm-12">
          <LoadingButton
            isLoading={isLoading}
            disabled={!isValid}
            data-cy="settings-saveNotificationButton"
            loadingText="Saving..."
          >
            Save notification settings
          </LoadingButton>
        </div>
      </div>
    </Form>
  );
}
