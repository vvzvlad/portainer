import { SchemaOf, boolean, mixed, object, string } from 'yup';

import { AutoUpdateScope } from '../../types';

import { Values } from './types';

// Matches Go's time.ParseDuration units (e.g. "6h", "30m", "1h30m").
const durationPattern = /^(\d+(\.\d+)?(ns|us|µs|ms|s|m|h))+$/;

export function validation(): SchemaOf<Values> {
  return object({
    enabled: boolean().default(false),
    pollInterval: string()
      .required('Poll interval is required')
      .matches(durationPattern, 'Must be a valid duration (e.g. 6h, 30m, 1h30m)'),
    scope: mixed<AutoUpdateScope>()
      .oneOf(['labeled', 'all'])
      .required('Scope is required'),
    cleanup: boolean().default(false),
  });
}
