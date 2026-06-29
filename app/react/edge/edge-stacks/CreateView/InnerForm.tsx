import { Form, useFormikContext } from 'formik';

import { applySetStateAction } from '@/react-tools/apply-set-state-action';
import { EnvironmentType } from '@/react/portainer/environments/types';

import { FormActions } from '@@/form-components/FormActions';

import { EdgeGroupsSelector } from '../components/EdgeGroupsSelector';
import { EdgeStackDeploymentTypeSelector } from '../components/EdgeStackDeploymentTypeSelector';
import { useEdgeGroupHasType } from '../ItemView/EditEdgeStackForm/useEdgeGroupHasType';
import { DeploymentType } from '../types';

import { DockerComposeForm } from './DockerComposeForm';
import { KubeFormValues, KubeManifestForm } from './KubeManifestForm';
import { NameField } from './NameField';
import { FormValues } from './types';

export function InnerForm({
  webhookId,
  isLoading,
  onChangeTemplate,
}: {
  webhookId: string;
  isLoading: boolean;
  onChangeTemplate: ({
    templateType,
    templateId,
  }: {
    templateType: 'app' | 'custom' | undefined;
    templateId: number | undefined;
  }) => void;
}) {
  const { values, setFieldValue, errors, setValues, isValid } =
    useFormikContext<FormValues>();
  const { hasType } = useEdgeGroupHasType(values.groupIds);

  const hasKubeEndpoint = hasType(EnvironmentType.EdgeAgentOnKubernetes);
  const hasDockerEndpoint = hasType(EnvironmentType.EdgeAgentOnDocker);
  const hasMultipleTypes = hasKubeEndpoint && hasDockerEndpoint;
  const multipleTypesError = hasMultipleTypes
    ? `There are no available deployment types when there is more than one
          type of environment in your edge group selection (e.g. Kubernetes and
          Docker environments). Please select edge groups that have environments
          of the same type.`
    : undefined;

  return (
    <Form className="form-horizontal" role="form">
      <NameField
        onChange={(value) => setFieldValue('name', value)}
        value={values.name}
        errors={errors.name}
        placeholder="e.g. my-stack"
      />

      <EdgeGroupsSelector
        value={values.groupIds}
        onChange={(value) => setFieldValue('groupIds', value)}
        error={errors.groupIds || multipleTypesError}
      />

      <EdgeStackDeploymentTypeSelector
        value={values.deploymentType}
        hasDockerEndpoint={hasDockerEndpoint}
        hasKubeEndpoint={hasKubeEndpoint}
        onChange={(value) => setFieldValue('deploymentType', value)}
        error={errors.deploymentType}
      />

      {values.deploymentType === DeploymentType.Compose && (
        <DockerComposeForm
          webhookId={webhookId}
          onChangeTemplate={onChangeTemplate}
        />
      )}

      {values.deploymentType === DeploymentType.Kubernetes && (
        <KubeManifestForm
          values={values as KubeFormValues}
          webhookId={webhookId}
          errors={errors}
          setValues={(kubeValues) =>
            setValues((values) => ({
              ...values,
              ...applySetStateAction(kubeValues, values as KubeFormValues),
            }))
          }
        />
      )}

      <FormActions
        data-cy="edgeStackCreate-createStackButton"
        submitLabel="Deploy the stack"
        loadingText="Deployment in progress..."
        isValid={isValid}
        isLoading={isLoading}
      />
    </Form>
  );
}
