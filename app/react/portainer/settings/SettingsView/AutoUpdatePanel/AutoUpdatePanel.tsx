import { RefreshCw } from 'lucide-react';
import { Field, Form, Formik, useFormikContext } from 'formik';

import { notifySuccess } from '@/portainer/services/notifications';

import { Widget } from '@@/Widget';
import { LoadingButton } from '@@/buttons';
import { FormControl } from '@@/form-components/FormControl';
import { Input, Select } from '@@/form-components/Input';
import { SwitchField } from '@@/form-components/SwitchField';
import { TextTip } from '@@/Tip/TextTip';

import { useSettings, useUpdateSettingsMutation } from '../../queries';
import { AutoUpdateScope } from '../../types';

import { Values } from './types';
import { validation } from './validation';

const scopeOptions: Array<{ value: AutoUpdateScope; label: string }> = [
  { value: 'labeled', label: 'Labeled containers only' },
  { value: 'all', label: 'All containers' },
];

export function AutoUpdatePanel() {
  const settingsQuery = useSettings((settings) => settings.ContainerAutomation);
  const mutation = useUpdateSettingsMutation();

  if (!settingsQuery.data) {
    return null;
  }

  const autoUpdate = settingsQuery.data.AutoUpdate;
  const initialValues: Values = {
    enabled: autoUpdate.Enabled,
    pollInterval: autoUpdate.PollInterval || '6h',
    scope: autoUpdate.Scope || 'labeled',
    cleanup: autoUpdate.Cleanup,
    rollbackOnFailure: autoUpdate.RollbackOnFailure,
    rollbackTimeout: autoUpdate.RollbackTimeout || '120s',
  };

  return (
    <Widget>
      <Widget.Title icon={RefreshCw} title="Container auto-update" />
      <Widget.Body>
        <div className="mb-3">
          <TextTip color="blue">
            When enabled, Portainer periodically checks running containers for a
            newer image, replacing the containrrr/watchtower sidecar. Standalone
            containers are recreated with a re-pull, and containers belonging to a
            Portainer file-based (non-git) compose stack are updated by redeploying
            their stack so they stay part of it. Git-backed stacks and
            externally-managed containers are detection-only here: a newer image is
            reported but applied through their own flow (the next git change or a
            manual &quot;Update now&quot;), not by this daemon. Per-container opt-in
            is controlled with the <code>io.portainer.update.enable=true</code>{' '}
            label; add <code>io.portainer.update.monitor-only=true</code> to detect
            updates without applying them.
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
          AutoUpdate: {
            Enabled: values.enabled,
            PollInterval: values.pollInterval,
            Scope: values.scope,
            Cleanup: values.cleanup,
            RollbackOnFailure: values.rollbackOnFailure,
            RollbackTimeout: values.rollbackTimeout,
          },
        },
      },
      {
        onSuccess() {
          notifySuccess('Success', 'Auto-update settings updated');
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
            label="Enable auto-update"
            checked={values.enabled}
            name="enabled"
            onChange={(value) => setFieldValue('enabled', value)}
            labelClass="col-sm-3 col-lg-2"
            data-cy="settings-autoUpdateEnabled"
          />
        </div>
      </div>

      <FormControl
        label="Poll interval"
        inputId="autoupdate_poll_interval"
        errors={errors.pollInterval}
        required
      >
        <Field
          as={Input}
          id="autoupdate_poll_interval"
          placeholder="e.g. 6h"
          name="pollInterval"
          disabled={!values.enabled}
          data-cy="settings-autoUpdatePollInterval"
        />
      </FormControl>

      <FormControl
        label="Scope"
        inputId="autoupdate_scope"
        errors={errors.scope}
        required
      >
        <Field
          as={Select}
          id="autoupdate_scope"
          name="scope"
          options={scopeOptions}
          disabled={!values.enabled}
          data-cy="settings-autoUpdateScope"
        />
      </FormControl>

      <div className="form-group">
        <div className="col-sm-12">
          <SwitchField
            label="Remove dangling old images after update"
            checked={values.cleanup}
            name="cleanup"
            onChange={(value) => setFieldValue('cleanup', value)}
            labelClass="col-sm-3 col-lg-2"
            disabled={!values.enabled}
            data-cy="settings-autoUpdateCleanup"
          />
        </div>
      </div>

      <div className="form-group">
        <div className="col-sm-12">
          <SwitchField
            label="Roll back on failed health check"
            tooltip="When a standalone container with a healthcheck does not become healthy within the rollback timeout after an update, it is recreated on its previous image. Stack-managed containers are not rolled back."
            checked={values.rollbackOnFailure}
            name="rollbackOnFailure"
            onChange={(value) => setFieldValue('rollbackOnFailure', value)}
            labelClass="col-sm-3 col-lg-2"
            disabled={!values.enabled}
            data-cy="settings-autoUpdateRollback"
          />
        </div>
      </div>

      <FormControl
        label="Rollback timeout"
        inputId="autoupdate_rollback_timeout"
        errors={errors.rollbackTimeout}
        required
      >
        <Field
          as={Input}
          id="autoupdate_rollback_timeout"
          placeholder="e.g. 120s"
          name="rollbackTimeout"
          disabled={!values.enabled || !values.rollbackOnFailure}
          data-cy="settings-autoUpdateRollbackTimeout"
        />
      </FormControl>

      <div className="form-group">
        <div className="col-sm-12">
          <LoadingButton
            isLoading={isLoading}
            disabled={!isValid}
            data-cy="settings-saveAutoUpdateButton"
            loadingText="Saving..."
          >
            Save auto-update settings
          </LoadingButton>
        </div>
      </div>
    </Form>
  );
}
