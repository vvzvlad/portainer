import { boolean, object, SchemaOf, string } from 'yup';

import { FormValues } from './types';

export function validationSchema(): SchemaOf<FormValues> {
  return object()
    .shape({
      EnableEdgeComputeFeatures: boolean().required('This field is required.'),
      EnforceEdgeID: boolean().required('This field is required.'),
    })
    .concat(
      object({
        EdgePortainerUrl: string().default(''),
        Edge: object({
          TunnelServerAddress: string().default(''),
        }),
      })
    );
}
