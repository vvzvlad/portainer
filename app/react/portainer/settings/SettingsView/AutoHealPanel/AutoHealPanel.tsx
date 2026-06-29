import { HeartPulse } from 'lucide-react';
import { Field, Form, Formik, useFormikContext } from 'formik';

import { notifySuccess } from '@/portainer/services/notifications';

import { Widget } from '@@/Widget';
import { LoadingButton } from '@@/buttons';
import { FormControl } from '@@/form-components/FormControl';
import { Input, Select } from '@@/form-components/Input';
import { SwitchField } from '@@/form-components/SwitchField';
import { TextTip } from '@@/Tip/TextTip';

import { useSettings, useUpdateSettingsMutation } from '../../queries';
import { AutoHealScope } from '../../types';

import { Values } from './types';
import { validation } from './validation';

const scopeOptions: Array<{ value: AutoHealScope; label: string }> = [
  { value: 'labeled', label: 'Labeled containers only' },
  { value: 'all', label: 'All containers' },
];

export function AutoHealPanel() {
  const settingsQuery = useSettings((settings) => settings.ContainerAutomation);
  const mutation = useUpdateSettingsMutation();

  if (!settingsQuery.data) {
    return null;
  }

  const autoHeal = settingsQuery.data.AutoHeal;
  const initialValues: Values = {
    enabled: autoHeal.Enabled,
    checkInterval: autoHeal.CheckInterval || '30s',
    scope: autoHeal.Scope || 'labeled',
  };

  return (
    <Widget>
      <Widget.Title icon={HeartPulse} title="Container auto-heal" />
      <Widget.Body>
        <div className="mb-3">
          <TextTip color="blue">
            When enabled, Portainer periodically restarts containers whose Docker
            healthcheck reports an unhealthy state, replacing the
            willfarrell/autoheal sidecar. Per-container opt-in is controlled with
            the <code>io.portainer.autoheal.enable=true</code> label.
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
          AutoHeal: {
            Enabled: values.enabled,
            CheckInterval: values.checkInterval,
            Scope: values.scope,
          },
        },
      },
      {
        onSuccess() {
          notifySuccess('Success', 'Auto-heal settings updated');
        },
      }
    );
  }
}

function InnerForm({ isLoading }: { isLoading: boolean }) {
  const { values, setFieldValue, isValid, errors } = useFormikContext<Values>();

  return (
    <Form className="form-horizontal">
      <div className="form-group">
        <div className="col-sm-12">
          <SwitchField
            label="Enable auto-heal"
            checked={values.enabled}
            name="enabled"
            onChange={(value) => setFieldValue('enabled', value)}
            labelClass="col-sm-3 col-lg-2"
            data-cy="settings-autoHealEnabled"
          />
        </div>
      </div>

      <FormControl
        label="Check interval"
        inputId="autoheal_check_interval"
        errors={errors.checkInterval}
        required
      >
        <Field
          as={Input}
          id="autoheal_check_interval"
          placeholder="e.g. 30s"
          name="checkInterval"
          disabled={!values.enabled}
          data-cy="settings-autoHealCheckInterval"
        />
      </FormControl>

      <FormControl
        label="Scope"
        inputId="autoheal_scope"
        errors={errors.scope}
        required
      >
        <Field
          as={Select}
          id="autoheal_scope"
          name="scope"
          options={scopeOptions}
          disabled={!values.enabled}
          data-cy="settings-autoHealScope"
        />
      </FormControl>

      <div className="form-group">
        <div className="col-sm-12">
          <LoadingButton
            isLoading={isLoading}
            disabled={!isValid}
            data-cy="settings-saveAutoHealButton"
            loadingText="Saving..."
          >
            Save auto-heal settings
          </LoadingButton>
        </div>
      </div>
    </Form>
  );
}
