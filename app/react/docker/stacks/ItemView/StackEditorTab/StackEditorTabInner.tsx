import { Form, useFormikContext } from 'formik';
import { JSONSchema7 } from 'json-schema';
import { useCallback } from 'react';

import {
  Stack,
  StackFileVersionInfo,
  StackType,
} from '@/react/common/stacks/types';
import { PruneField } from '@/react/common/stacks/PruneField';
import { EnvironmentType } from '@/react/portainer/environments/types';
import { Authorized, useAuthorizations } from '@/react/hooks/useUser';

import { CodeEditor } from '@@/CodeEditor';
import { StackEnvironmentVariablesPanel } from '@@/form-components/EnvironmentVariablesFieldset';
import { FormActions } from '@@/form-components/FormActions';
import { usePreventExit } from '@@/WebEditorForm';
import { FormError } from '@@/form-components/FormError';

import { WebhookFieldset } from '../../common/WebhookFieldset';

import { StackEditorFormValues } from './StackEditorTab.types';
import { useVersionedStackFile } from './useVersionedStackFile';

/**
 * Decide the `rollbackTo` value for a version chosen in the selector.
 *
 * `versions[0]` is the latest/current version (the list is sorted descending).
 * Picking the current version is NOT a rollback, so this returns `undefined`
 * to keep the normal edit-and-deploy flow; only a genuinely older version sets
 * a rollback target (which the backend reads from disk, ignoring the buffer).
 */
export function resolveRollbackTarget(
  newVersion: number,
  versions?: Array<number>
): number | undefined {
  if (!versions || versions.length <= 1) {
    return undefined;
  }
  return newVersion < versions[0] ? newVersion : undefined;
}

interface StackEditorTabInnerProps {
  stackType: StackType | undefined;
  composeSyntaxMaxVersion: number;
  envType: EnvironmentType;
  schema: JSONSchema7;
  isOrphaned: boolean;
  versions?: Array<number>;
  versionsInfo?: StackFileVersionInfo[];
  stackId: Stack['Id'];
  isSaved: boolean;
  isSubmitting: boolean;
  webhookId: string;
}

export function StackEditorTabInner({
  stackType,
  composeSyntaxMaxVersion,
  envType,
  schema,
  isOrphaned,
  versions,
  versionsInfo,
  stackId,
  isSaved,
  isSubmitting,
  webhookId,
}: StackEditorTabInnerProps) {
  const { authorized: isAuthorizedToUpdate } = useAuthorizations(
    'PortainerStackUpdate'
  );

  const { values, errors, setFieldValue, isValid, initialValues } =
    useFormikContext<StackEditorFormValues>();

  usePreventExit(
    initialValues.stackFileContent,
    values.stackFileContent,
    !isSubmitting && !isSaved
  );

  const handleLoadFile = useCallback(
    (content: string) => {
      setFieldValue('stackFileContent', content);
    },
    [setFieldValue]
  );

  const handleContentChange = useCallback(
    (value: string) => {
      setFieldValue('stackFileContent', value);
      // A manual edit means the user wants a normal edit-and-deploy, not a
      // rollback, so clear rollbackTo (otherwise the backend would ignore the
      // edited buffer and redeploy the selected version from disk). This runs
      // only on genuine user input: the programmatic version-load goes through
      // handleLoadFile -> setFieldValue, which updates the editor's `value`
      // prop and is skipped by CodeMirror's onChange (marked as an external
      // change), so it does not clear rollbackTo.
      setFieldValue('rollbackTo', undefined);
    },
    [setFieldValue]
  );

  useVersionedStackFile({
    stackId,
    version: values.rollbackTo,
    onLoad: handleLoadFile,
  });

  const isDeployDisabled = isOrphaned;

  return (
    <Form className="form-horizontal">
      {/* Docker Compose Info Section */}
      <div className="form-group mb-0 space-y-2">
        {stackType === StackType.DockerCompose &&
          composeSyntaxMaxVersion === 2 && (
            <span className="col-sm-12 text-muted small">
              This stack will be deployed using the equivalent of{' '}
              <code>docker compose</code>. Only Compose file format version{' '}
              <b>2</b> is supported at the moment.
            </span>
          )}
        {stackType === StackType.DockerCompose &&
          composeSyntaxMaxVersion > 2 && (
            <span className="col-sm-12 text-muted small">
              This stack will be deployed using <code>docker compose</code>.
            </span>
          )}
        <span className="col-sm-12 text-muted small">
          You can get more information about Compose file format in the{' '}
          <a
            href="https://docs.docker.com/compose/compose-file/"
            target="_blank"
            rel="noreferrer"
          >
            official documentation
          </a>
          .
        </span>
        <div className="col-sm-12">
          {errors.stackFileContent && (
            <FormError>{errors.stackFileContent}</FormError>
          )}
        </div>
      </div>

      <div className="form-group">
        <div className="col-sm-12">
          <CodeEditor
            id="stack-editor"
            textTip="Define or paste the content of your docker compose file here"
            type="yaml"
            onChange={handleContentChange}
            value={values.stackFileContent}
            readonly={isOrphaned || !isAuthorizedToUpdate}
            schema={schema}
            data-cy="stack-editor"
            onVersionChange={handleVersionChange}
            versions={versions}
            versionsInfo={versionsInfo}
          />
        </div>
      </div>

      <StackEnvironmentVariablesPanel
        values={values.environmentVariables}
        onChange={(envVars) => setFieldValue('environmentVariables', envVars)}
        errors={errors.environmentVariables}
        showHelpMessage
        isFoldable
      />

      {envType !== EnvironmentType.EdgeAgentOnDocker && (
        <WebhookFieldset
          onChange={(value) => setFieldValue('enabledWebhook', value)}
          value={values.enabledWebhook}
          webhookId={webhookId}
        />
      )}

      <Authorized authorizations="PortainerStackUpdate">
        <PruneField
          stackType={stackType}
          checked={values.prune}
          onChange={(checked) => setFieldValue('prune', checked)}
        />
      </Authorized>

      <Authorized authorizations="PortainerStackUpdate">
        <FormActions
          isValid={isValid && !isDeployDisabled}
          isLoading={isSubmitting}
          loadingText="Deployment in progress..."
          submitLabel="Update the stack"
          data-cy="stack-deploy-button"
        />
      </Authorized>
    </Form>
  );

  async function handleVersionChange(newVersion: number) {
    if (versions && versions.length > 1) {
      // Picking the current/top version clears rollbackTo (undefined); only a
      // genuinely older version sets it. See resolveRollbackTarget.
      const rollbackTarget = resolveRollbackTarget(newVersion, versions);
      if (rollbackTarget === undefined) {
        // Returning to the current version: restore the current file content so
        // the editor matches the selector. Otherwise the previously-viewed older
        // version's content would remain in the buffer (useVersionedStackFile
        // does not fetch when rollbackTo is undefined) and get deployed as a new
        // version. initialValues.stackFileContent is the current stack file
        // loaded at page init. This flows through the CodeEditor `value` prop as
        // an external change, so it does not re-trigger handleContentChange and
        // rollbackTo stays undefined.
        setFieldValue('stackFileContent', initialValues.stackFileContent);
      }
      setFieldValue('rollbackTo', rollbackTarget);
    }
  }
}
