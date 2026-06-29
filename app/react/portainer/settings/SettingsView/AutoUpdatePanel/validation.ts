import { SchemaOf, boolean, mixed, object, string } from 'yup';

import { AutoUpdateScope } from '../../types';

import { Values } from './types';

// Matches Go's time.ParseDuration units (e.g. "6h", "30m", "1h30m").
const durationPattern = /^(\d+(\.\d+)?(ns|us|µs|ms|s|m|h))+$/;

// Lower bound for the poll interval, kept in sync with the backend
// (minAutoUpdatePollInterval). Polling more often than this only adds registry
// load: the image-status cache is long-lived, so a sub-minute interval is wasteful.
const minPollIntervalSeconds = 60;

// Seconds per Go duration unit, used to evaluate the configured interval against
// the floor. Mirrors the units accepted by durationPattern.
const unitSeconds: Record<string, number> = {
  ns: 1e-9,
  us: 1e-6,
  µs: 1e-6,
  ms: 1e-3,
  s: 1,
  m: 60,
  h: 3600,
};

// parseGoDurationSeconds converts a Go-style duration (e.g. "1h30m") to seconds,
// or returns null when the string is not a well-formed duration.
function parseGoDurationSeconds(value: string): number | null {
  if (!durationPattern.test(value)) {
    return null;
  }

  let total = 0;
  const componentPattern = /(\d+(?:\.\d+)?)(ns|us|µs|ms|s|m|h)/g;
  let match = componentPattern.exec(value);
  while (match !== null) {
    total += parseFloat(match[1]) * unitSeconds[match[2]];
    match = componentPattern.exec(value);
  }

  return total;
}

export function validation(): SchemaOf<Values> {
  return object({
    enabled: boolean().default(false),
    pollInterval: string()
      .required('Poll interval is required')
      .matches(durationPattern, 'Must be a valid duration (e.g. 6h, 30m, 1h30m)')
      .test(
        'min-poll-interval',
        'Poll interval must be at least 1m',
        (value) => {
          if (!value) {
            return true; // let required/matches report the error first
          }

          const seconds = parseGoDurationSeconds(value);
          if (seconds === null) {
            return true; // let matches report the format error first
          }

          return seconds >= minPollIntervalSeconds;
        }
      ),
    scope: mixed<AutoUpdateScope>()
      .oneOf(['labeled', 'all'])
      .required('Scope is required'),
    cleanup: boolean().default(false),
    rollbackOnFailure: boolean().default(false),
    rollbackTimeout: string()
      .required('Rollback timeout is required')
      .matches(durationPattern, 'Must be a valid duration (e.g. 120s, 2m)')
      .test(
        'positive-rollback-timeout',
        'Rollback timeout must be positive',
        (value) => {
          if (!value) {
            return true; // let required/matches report the error first
          }

          const seconds = parseGoDurationSeconds(value);
          if (seconds === null) {
            return true; // let matches report the format error first
          }

          return seconds > 0;
        }
      ),
  });
}
