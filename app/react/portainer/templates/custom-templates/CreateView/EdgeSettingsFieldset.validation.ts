import { SchemaOf, mixed } from 'yup';

import { EdgeTemplateSettings } from '@/react/portainer/templates/custom-templates/types';

export function edgeFieldsetValidation(): SchemaOf<EdgeTemplateSettings> {
  return mixed().default(undefined) as SchemaOf<EdgeTemplateSettings>;
}
