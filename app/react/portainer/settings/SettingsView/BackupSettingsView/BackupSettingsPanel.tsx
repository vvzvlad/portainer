import { Download } from 'lucide-react';

import { Widget, WidgetBody, WidgetTitle } from '@@/Widget';
import { FormSection } from '@@/form-components/FormSection';

import { BackupFileForm } from './BackupFileForm';

export function BackupSettingsPanel() {
  return (
    <Widget>
      <WidgetTitle icon={Download} title="Back up Portainer" />
      <WidgetBody>
        <div className="form-horizontal">
          <FormSection title="Backup configuration">
            <div className="form-group col-sm-12 text-muted small">
              This will back up your Portainer server configuration and does not
              include containers.
            </div>

            <BackupFileForm />
          </FormSection>
        </div>
      </WidgetBody>
    </Widget>
  );
}
