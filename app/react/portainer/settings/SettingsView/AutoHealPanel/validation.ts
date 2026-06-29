import { SchemaOf, boolean, mixed, object, string } from 'yup';

import { AutoHealScope } from '../../types';

import { Values } from './types';

// Matches Go's time.ParseDuration units (e.g. "30s", "1m30s", "2h").
const durationPattern = /^(\d+(\.\d+)?(ns|us|µs|ms|s|m|h))+$/;

export function validation(): SchemaOf<Values> {
  return object({
    enabled: boolean().default(false),
    checkInterval: string()
      .required('Check interval is required')
      .matches(
        durationPattern,
        'Must be a valid duration (e.g. 30s, 1m, 2h)'
      ),
    scope: mixed<AutoHealScope>()
      .oneOf(['labeled', 'all'])
      .required('Scope is required'),
  });
}
