import { Form, Formik, useFormikContext } from 'formik';
import { useState, useEffect } from 'react';
import { array, boolean, number, object, SchemaOf, string } from 'yup';
import { useRouter } from '@uirouter/react';
import _ from 'lodash';

import { EdgeGroupsSelector } from '@/react/edge/edge-stacks/components/EdgeGroupsSelector';
import { EdgeStackDeploymentTypeSelector } from '@/react/edge/edge-stacks/components/EdgeStackDeploymentTypeSelector';
import { DeploymentType, EdgeStack } from '@/react/edge/edge-stacks/types';
import { EnvironmentType } from '@/react/portainer/environments/types';
import { createWebhookId } from '@/portainer/helpers/webhookHelper';
import { notifySuccess } from '@/portainer/services/notifications';

import { FormSection } from '@@/form-components/FormSection';
import { TextTip } from '@@/Tip/TextTip';
import { LoadingButton } from '@@/buttons';
import { FormError } from '@@/form-components/FormError';
import { envVarValidation } from '@@/form-components/EnvironmentVariablesFieldset';
import { usePreventExit } from '@@/WebEditorForm';

import {
  getEdgeStackFile,
  useEdgeStackFile,
} from '../../queries/useEdgeStackFile';
import { staggerConfigValidation } from '../../components/StaggerFieldset';
import { getDefaultStaggerConfig } from '../../components/StaggerFieldset.types';

import { FormValues } from './types';
import { useEdgeGroupHasType } from './useEdgeGroupHasType';
import { useUpdateEdgeStackMutation } from './useUpdateEdgeStackMutation';
import { ComposeForm } from './ComposeForm';
import { KubernetesForm } from './KubernetesForm';
import { useAllowKubeToSelectCompose } from './useAllowKubeToSelectCompose';

const forms = {
  [DeploymentType.Compose]: ComposeForm,
  [DeploymentType.Kubernetes]: KubernetesForm,
};

export function NonGitStackForm({ edgeStack }: { edgeStack: EdgeStack }) {
  const mutation = useUpdateEdgeStackMutation();
  const fileQuery = useEdgeStackFile(edgeStack.Id, { skipErrors: true });
  const allowKubeToSelectCompose = useAllowKubeToSelectCompose(edgeStack);
  const router = useRouter();
  const [webhookId] = useState(() => edgeStack.Webhook || createWebhookId());

  if (!fileQuery.isSuccess) {
    return null;
  }

  const fileContent = fileQuery.data || '';

  const formValues: FormValues = {
    edgeGroups: edgeStack.EdgeGroups,
    deploymentType: edgeStack.DeploymentType,
    privateRegistryId: edgeStack.Registries?.[0],
    content: fileContent,
    useManifestNamespaces: edgeStack.UseManifestNamespaces,
    prePullImage: edgeStack.PrePullImage ?? false,
    retryDeploy: edgeStack.RetryDeploy ?? false,
    webhookEnabled: !!edgeStack.Webhook,
    envVars: edgeStack.EnvVars || [],
    rollbackTo: undefined,
    staggerConfig: edgeStack.StaggerConfig || getDefaultStaggerConfig(),
  };

  const versionOptions = undefined;

  return (
    <Formik
      initialValues={formValues}
      onSubmit={handleSubmit}
      validationSchema={formValidation()}
    >
      <InnerForm
        edgeStack={edgeStack}
        isLoading={mutation.isLoading}
        allowKubeToSelectCompose={allowKubeToSelectCompose}
        versionOptions={versionOptions}
        isSaved={mutation.isSuccess}
      />
    </Formik>
  );

  async function handleSubmit(values: FormValues) {
    const rePullImage = false;

    const updateVersion = !!(
      fileContent !== values.content ||
      values.privateRegistryId !== edgeStack.Registries?.[0] ||
      values.useManifestNamespaces !== edgeStack.UseManifestNamespaces ||
      values.prePullImage !== edgeStack.PrePullImage ||
      values.retryDeploy !== edgeStack.RetryDeploy ||
      !edgeStack.EnvVars ||
      _.differenceWith(values.envVars, edgeStack.EnvVars, _.isEqual).length >
        0 ||
      rePullImage
    );

    mutation.mutate(
      {
        id: edgeStack.Id,
        stackFileContent: values.content,
        edgeGroups: values.edgeGroups,
        deploymentType: values.deploymentType,
        registries: values.privateRegistryId ? [values.privateRegistryId] : [],
        useManifestNamespaces: values.useManifestNamespaces,
        prePullImage: values.prePullImage,
        rePullImage,
        retryDeploy: values.retryDeploy,
        updateVersion,
        webhook: values.webhookEnabled ? webhookId : '',
        envVars: values.envVars,
        rollbackTo: values.rollbackTo,
        staggerConfig: values.staggerConfig,
      },
      {
        onSuccess: () => {
          notifySuccess('Success', 'Stack successfully deployed');
          router.stateService.go('^');
        },
      }
    );
  }
}

function InnerForm({
  edgeStack,
  isLoading,
  allowKubeToSelectCompose,
  versionOptions,
  isSaved,
}: {
  edgeStack: EdgeStack;
  isLoading: boolean;
  allowKubeToSelectCompose: boolean;
  versionOptions: number[] | undefined;
  isSaved: boolean;
}) {
  const { values, setFieldValue, isValid, errors, initialValues } =
    useFormikContext<FormValues>();

  usePreventExit(initialValues.content, values.content, !isSaved);

  const { getCachedContent, setContentCache } = useCachedContent();
  const { hasType } = useEdgeGroupHasType(values.edgeGroups);
  const [selectedVersion, setSelectedVersion] = useState(versionOptions?.[0]);

  useEffect(() => {
    if (versionOptions && selectedVersion !== versionOptions[0]) {
      setFieldValue('rollbackTo', selectedVersion);
    } else {
      setFieldValue('rollbackTo', undefined);
    }
  }, [selectedVersion, setFieldValue, versionOptions]);

  const hasKubeEndpoint = hasType(EnvironmentType.EdgeAgentOnKubernetes);
  const hasDockerEndpoint = hasType(EnvironmentType.EdgeAgentOnDocker);

  const DeploymentForm = forms[values.deploymentType];

  return (
    <Form className="form-horizontal">
      <EdgeGroupsSelector
        value={values.edgeGroups}
        onChange={(value) => setFieldValue('edgeGroups', value)}
        error={errors.edgeGroups}
      />

      {hasKubeEndpoint && hasDockerEndpoint && (
        <TextTip>
          There are no available deployment types when there is more than one
          type of environment in your edge group selection (e.g. Kubernetes and
          Docker environments). Please select edge groups that have environments
          of the same type.
        </TextTip>
      )}

      {values.deploymentType === DeploymentType.Compose && hasKubeEndpoint && (
        <FormError>
          Edge groups with kubernetes environments no longer support compose
          deployment types in Portainer. Please select edge groups that only
          have docker environments when using compose deployment types.
        </FormError>
      )}

      <EdgeStackDeploymentTypeSelector
        allowKubeToSelectCompose={allowKubeToSelectCompose}
        value={values.deploymentType}
        hasDockerEndpoint={hasType(EnvironmentType.EdgeAgentOnDocker)}
        hasKubeEndpoint={hasType(EnvironmentType.EdgeAgentOnKubernetes)}
        onChange={(value) => {
          setFieldValue('content', getCachedContent(value));
          setFieldValue('deploymentType', value);
        }}
      />

      <DeploymentForm
        hasKubeEndpoint={hasType(EnvironmentType.EdgeAgentOnKubernetes)}
        handleContentChange={handleContentChange}
        versionOptions={versionOptions}
        handleVersionChange={handleVersionChange}
      />

      <FormSection title="Actions">
        <div className="form-group">
          <div className="col-sm-12">
            <LoadingButton
              className="!ml-0"
              data-cy="update-stack-button"
              size="small"
              disabled={!isValid}
              isLoading={isLoading}
              button-spinner="$ctrl.actionInProgress"
              loadingText="Update in progress..."
            >
              Update the stack
            </LoadingButton>
          </div>
        </div>
      </FormSection>
    </Form>
  );

  function handleContentChange(type: DeploymentType, content: string) {
    setFieldValue('content', content);
    setContentCache(type, content);
  }

  async function handleVersionChange(newVersion: number) {
    if (!versionOptions) {
      return;
    }

    const fileContent = await getEdgeStackFile(edgeStack.Id, newVersion).catch(
      () => ''
    );
    if (fileContent) {
      if (versionOptions.length > 1) {
        if (newVersion < versionOptions[0]) {
          setSelectedVersion(newVersion);
        } else {
          setSelectedVersion(versionOptions[0]);
        }
      }
      handleContentChange(values.deploymentType, fileContent);
    }
  }
}

function useCachedContent() {
  const [cachedContent, setCachedContent] = useState({
    [DeploymentType.Compose]: '',
    [DeploymentType.Kubernetes]: '',
  });

  function handleChangeContent(type: DeploymentType, content: string) {
    setCachedContent((cache) => ({ ...cache, [type]: content }));
  }

  return {
    setContentCache: handleChangeContent,
    getCachedContent: (type: DeploymentType) => cachedContent[type],
  };
}

function formValidation(): SchemaOf<FormValues> {
  return object({
    content: string().required('Content is required'),
    deploymentType: number()
      .oneOf([0, 1, 2])
      .required('Deployment type is required'),
    privateRegistryId: number().optional(),
    prePullImage: boolean().default(false),
    retryDeploy: boolean().default(false),
    useManifestNamespaces: boolean().default(false),
    edgeGroups: array()
      .of(number().required())
      .required()
      .min(1, 'At least one edge group is required'),
    webhookEnabled: boolean().default(false),
    versions: array().of(number().optional()).optional(),
    envVars: envVarValidation(),
    rollbackTo: number().optional(),
    staggerConfig: staggerConfigValidation(),
  });
}
