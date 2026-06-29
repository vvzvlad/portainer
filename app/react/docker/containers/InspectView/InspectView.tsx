import { useCurrentStateAndParams } from '@uirouter/react';
import { Circle, Code as CodeIcon, File } from 'lucide-react';
import { useState } from 'react';

import { trimContainerName } from '@/docker/filters/utils';
import { useEnvironmentId } from '@/react/hooks/useEnvironmentId';

import { JsonTree } from '@@/JsonTree';
import { PageHeader } from '@@/PageHeader';
import { Widget } from '@@/Widget';
import { ButtonSelector } from '@@/form-components/ButtonSelector/ButtonSelector';
import { Code } from '@@/Code';

import { useContainerInspect } from '../queries/useContainerInspect';
import { getContainerSubTabBreadcrumbs } from '../ItemView/containerBreadcrumbs';

export function InspectView() {
  const environmentId = useEnvironmentId();
  const { state, params } = useCurrentStateAndParams();
  const { id, nodeName } = params;
  const inspectQuery = useContainerInspect(environmentId, id, { nodeName });
  const [viewType, setViewType] = useState<'tree' | 'text'>('tree');

  if (!inspectQuery.data) {
    return null;
  }

  const containerInfo = inspectQuery.data;

  return (
    <>
      <PageHeader
        title="Container inspect"
        breadcrumbs={getContainerSubTabBreadcrumbs(
          state?.name,
          params,
          trimContainerName(containerInfo.Name),
          'Inspect'
        )}
      />

      <div className="row">
        <div className="col-lg-12 col-md-12 col-xs-12">
          <Widget>
            <Widget.Title icon={Circle} title="Inspect">
              <ButtonSelector<'tree' | 'text'>
                onChange={(value) => setViewType(value)}
                value={viewType}
                options={[
                  {
                    label: 'Tree',
                    icon: CodeIcon,
                    value: 'tree',
                  },
                  {
                    label: 'Text',
                    icon: File,
                    value: 'text',
                  },
                ]}
              />
            </Widget.Title>
            <Widget.Body>
              {viewType === 'text' && (
                <Code showCopyButton>
                  {JSON.stringify(containerInfo, undefined, 4)}
                </Code>
              )}
              {viewType === 'tree' && <JsonTree data={containerInfo} />}
            </Widget.Body>
          </Widget>
        </div>
      </div>
    </>
  );
}
