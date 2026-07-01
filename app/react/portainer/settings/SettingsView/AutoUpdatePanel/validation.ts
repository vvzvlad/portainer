import { SchemaOf, boolean, mixed, object, string } from 'yup';

import { AutoUpdateScope } from '../../types';
import { durationPattern, parseGoDurationSeconds } from '../parseGoDuration';

import { Values } from './types';

// Lower bound for the poll interval, kept in sync with the backend
// (minAutoUpdatePollInterval). Polling more often than this only adds registry
// load: the image-status cache (~5m) bounds detection latency, so a sub-minute
// interval is wasteful.
const minPollIntervalSeconds = 60;

// Lower bound for the rollback timeout, kept in sync with the backend
// (minAutoUpdateRollbackTimeout). A near-zero timeout would roll back before any
// container can pass its healthcheck, defeating the gate.
const minRollbackTimeoutSeconds = 10;

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
        'min-rollback-timeout',
        'Rollback timeout must be at least 10s',
        (value) => {
          if (!value) {
            return true; // let required/matches report the error first
          }

          const seconds = parseGoDurationSeconds(value);
          if (seconds === null) {
            return true; // let matches report the format error first
          }

          return seconds >= minRollbackTimeoutSeconds;
        }
      ),
  });
}
