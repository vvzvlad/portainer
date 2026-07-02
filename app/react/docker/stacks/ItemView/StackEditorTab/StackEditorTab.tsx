import { Formik } from 'formik';
import { useRouter } from '@uirouter/react';
import { useQueryClient } from '@tanstack/react-query';
import _ from 'lodash';
import { useState } from 'react';
import uuidv4 from 'uuid/v4';

import { Stack, StackType } from '@/react/common/stacks/types';
import { useStackVersions } from '@/react/common/stacks/queries/useStackVersions';
import { queryKeys } from '@/react/common/stacks/queries/query-keys';
import { useDockerComposeSchema } from '@/react/hooks/useDockerComposeSchema/useDockerComposeSchema';
import { useCurrentEnvironment } from '@/react/hooks/useCurrentEnvironment';
import { confirmStackUpdate } from '@/react/common/stacks/common/confirm-stack-update';
import { notifyError, notifySuccess } from '@/portainer/services/notifications';

import { useUpdateStackMutation } from '../../useUpdateStack';

import { StackEditorFormValues } from './StackEditorTab.types';
import { getValidationSchema } from './StackEditorTab.validation';
import { StackEditorTabInner } from './StackEditorTabInner';

interface StackEditorTabProps {
  isOrphaned: boolean;
  containerNames?: string[];
  originalContainerNames?: string[];
  stack: Stack;
  originalFileContent: string;

  onSubmitSuccess?(): void;
}

export function StackEditorTab({
  isOrphaned,

  containerNames = [],
  originalContainerNames = [],
  originalFileContent,
  onSubmitSuccess = () => {},
  stack,
}: StackEditorTabProps) {
  const router = useRouter();
  const queryClient = useQueryClient();
  const mutation = useUpdateStackMutation();
  const envQuery = useCurrentEnvironment();
  const schemaQuery = useDockerComposeSchema();
  const versionsQuery = useStackVersions(stack.Id, envQuery.data?.Id, {
    enabled: !!envQuery.data,
  });
  const [webhookId] = useState(() => stack.Webhook || uuidv4());

  if (!envQuery.data || !schemaQuery.data) {
    return null;
  }

  const versionsInfo = versionsQuery.data;
  // Build the full descending version list from the fetched history; fall back
  // to the current + previous deployment versions if the history is unavailable
  // so the editor keeps working (e.g. while loading or on error).
  const versions =
    versionsInfo && versionsInfo.length > 0
      ? versionsInfo.map((v) => v.Version).sort((a, b) => b - a)
      : _.compact([
          stack.StackFileVersion,
          stack.PreviousDeploymentInfo?.FileVersion,
        ]);

  const envType = envQuery.data?.Type;
  const composeSyntaxMaxVersion = parseFloat(
    envQuery.data?.ComposeSyntaxMaxVersion
  );

  const initialValues: StackEditorFormValues = {
    environmentVariables: stack.Env || [],
    prune: !!(stack.Option && stack.Option.Prune),
    stackFileContent: originalFileContent,
    enabledWebhook: !!stack.Webhook,
  };

  return (
    <Formik
      initialValues={initialValues}
      validationSchema={getValidationSchema(
        containerNames,
        originalContainerNames
      )}
      onSubmit={async (values) => {
        const response = await confirmStackUpdate(
          'Do you want to force an update of the stack?',
          stack.Type === StackType.DockerSwarm
        );

        if (!response) {
          return;
        }

        mutation.mutate(
          {
            stackId: stack.Id,
            environmentId: envQuery.data.Id,
            payload: {
              stackFileContent: values.stackFileContent,
              env: values.environmentVariables,
              prune: values.prune,
              webhook: values.enabledWebhook ? webhookId : undefined,
              repullImageAndRedeploy: response.repullImageAndRedeploy,
              rollbackTo: values.rollbackTo,
            },
          },
          {
            onSuccess() {
              notifySuccess('Success', 'Stack successfully deployed');
              // Refresh the version history and cached file so the selector
              // reflects the new deployment / rollback. Invalidate the file by
              // its 3-element prefix (['stacks', id, 'file']) so it matches
              // EVERY versioned file query — the real keys carry a params object
              // ({version, commitHash}) in the 4th slot, which stackFile(id)
              // with no params (undefined) would not partial-match.
              queryClient.invalidateQueries(queryKeys.stackVersions(stack.Id));
              queryClient.invalidateQueries([...queryKeys.stack(stack.Id), 'file']);
              router.stateService.reload();
              onSubmitSuccess();
            },
            onError(err) {
              notifyError('Failure', err as Error, 'Unable to create stack');
            },
          }
        );
      }}
      validateOnMount
      enableReinitialize
    >
      <StackEditorTabInner
        stackId={stack.Id}
        stackType={stack.Type}
        composeSyntaxMaxVersion={composeSyntaxMaxVersion}
        isOrphaned={isOrphaned}
        envType={envType}
        schema={schemaQuery.data}
        versions={versions}
        versionsInfo={versionsInfo}
        isSubmitting={mutation.isLoading}
        isSaved={mutation.isSuccess}
        webhookId={webhookId}
      />
    </Formik>
  );
}
