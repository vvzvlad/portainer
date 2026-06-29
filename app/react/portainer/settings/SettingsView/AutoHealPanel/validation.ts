import { SchemaOf, boolean, mixed, object, string } from 'yup';

import { AutoHealScope } from '../../types';

import { Values } from './types';

// Matches Go's time.ParseDuration units (e.g. "30s", "1m30s", "2h").
const durationPattern = /^(\d+(\.\d+)?(ns|us|µs|ms|s|m|h))+$/;

// Lower bound for the check interval, kept in sync with the backend
// (minAutoHealCheckInterval). A near-zero interval would hammer Docker with
// list/inspect calls for no benefit, and the backend rejects it (400), so the
// front-end must reject it too rather than letting a sub-second value through.
const minCheckIntervalSeconds = 1;

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
