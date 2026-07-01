import { useState } from 'react';
import { Zap } from 'lucide-react';

import {
  ContainerEngine,
  Environment,
} from '@/react/portainer/environments/types';
import { commandsTabs } from '@/react/edge/components/EdgeScriptForm/scripts';
import EdgeAgentStandardIcon from '@/react/edge/components/edge-agent-standard.svg?c';

import { BoxSelectorOption } from '@@/BoxSelector/types';
import { BoxSelector } from '@@/BoxSelector';
import { FormSection } from '@@/form-components/FormSection';
import { Badge } from '@@/Badge';
import { ExternalLink } from '@@/ExternalLink';
import { useDocsUrl } from '@@/PageHeader/ContextHelp';

import { AnalyticsStateKey } from '../types';
import { EdgeAgentTab } from '../shared/EdgeAgentTab';

import { AgentPanel } from './AgentPanel';

interface Props {
  onCreate(environment: Environment, analytics: AnalyticsStateKey): void;
}

type CreationType = 'edgeAgentStandard' | 'agent';

const primaryOptions: BoxSelectorOption<CreationType>[] = [
  {
    id: 'edgeAgentStandard',
    icon: EdgeAgentStandardIcon,
    iconType: 'badge',
    label: 'Edge Agent Standard',
    description: (
      <>
        <span>
          <Badge type="infoSecondary">Recommended</Badge>{' '}
          <Badge type="infoSecondary">Supports Policies</Badge>
        </span>
        <span className="mt-1 block">
          The remote environment will initiate connections to the Portainer
          server, with the ability to open a secure on-demand tunnel for
          real-time interaction. The Portainer server must be accessible from
          the Edge Agent environment.
        </span>
      </>
    ),
    value: 'edgeAgentStandard',
  },
];

const legacyOptions: BoxSelectorOption<CreationType>[] = [
  {
    id: 'agent_endpoint',
    icon: Zap,
    iconType: 'badge',
    label: 'Agent',
    value: 'agent',
    description:
      'The Portainer Server will initiate connections to the remote environment. The agent on the remote environment must be accessible from the Portainer server environment.',
  },
];

export function WizardKubernetes({ onCreate }: Props) {
  const edgeAgentDocsUrl = useDocsUrl(
    '/faqs/getting-started/why-do-we-recommend-using-the-edge-agent-instead-of-the-traditional-agent'
  );
  const [creationType, setCreationType] = useState<CreationType>(
    primaryOptions[0].value
  );

  const tab = getTab(creationType);

  return (
    <div className="form-horizontal">
      <BoxSelector
        onChange={(v) => setCreationType(v)}
        options={primaryOptions}
        value={creationType}
        radioName="creation-type"
        className="!-mb-2"
      />

      <FormSection
        key="legacy-options"
        title="More options"
        titleSize="sm"
        isFoldable
        defaultFolded={false}
        className="mb-8"
      >
        <p className="text-muted mb-2 text-xs">
          These are legacy options that don&apos;t support edge features or
          policy management. For most use cases,{' '}
          <ExternalLink
            to={edgeAgentDocsUrl}
            data-cy="wizard-edge-agent-docs-link"
          >
            the Edge Agent is recommended
          </ExternalLink>
        </p>
        <BoxSelector
          onChange={(v) => setCreationType(v)}
          options={legacyOptions}
          value={creationType}
          radioName="creation-type"
        />
      </FormSection>

      {tab}
    </div>
  );

  function getTab(type: CreationType) {
    switch (type) {
      case 'agent':
        return (
          <AgentPanel
            onCreate={(environment) => onCreate(environment, 'kubernetesAgent')}
          />
        );
      case 'edgeAgentStandard':
        return (
          <EdgeAgentTab
            onCreate={(environment) =>
              onCreate(environment, 'kubernetesEdgeAgentStandard')
            }
            commands={[{ ...commandsTabs.k8sLinux, label: 'Linux' }]}
            containerEngine={ContainerEngine.Kubernetes}
          />
        );
      default:
        throw new Error('Creation type not supported');
    }
  }
}
