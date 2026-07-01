import { SchemaOf, boolean, mixed, object, string } from 'yup';

import { AutoHealScope } from '../../types';
import { durationPattern, parseGoDurationSeconds } from '../parseGoDuration';

import { Values } from './types';

// Lower bound for the check interval, kept in sync with the backend
// (minAutoHealCheckInterval). A near-zero interval would hammer Docker with
// list/inspect calls for no benefit, and the backend rejects it (400), so the
// front-end must reject it too rather than letting a sub-second value through.
const minCheckIntervalSeconds = 1;

export function validation(): SchemaOf<Values> {
  return object({
    enabled: boolean().default(false),
    checkInterval: string()
      .required('Check interval is required')
      .matches(durationPattern, 'Must be a valid duration (e.g. 30s, 1m, 2h)')
      .test(
        'min-check-interval',
        'Check interval must be at least 1s',
        (value) => {
          if (!value) {
            return true; // let required/matches report the error first
          }

          const seconds = parseGoDurationSeconds(value);
          if (seconds === null) {
            return true; // let matches report the format error first
          }

          return seconds >= minCheckIntervalSeconds;
        }
      ),
    scope: mixed<AutoHealScope>()
      .oneOf(['labeled', 'all'])
      .required('Scope is required'),
  });
}
