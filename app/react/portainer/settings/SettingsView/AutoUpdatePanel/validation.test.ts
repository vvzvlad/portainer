import { validation } from './validation';
import { Values } from './types';

function validate(values: Values) {
  return validation()
    .validate(values, { abortEarly: false })
    .then(() => undefined)
    .catch((err: { errors: string[] }) => err.errors);
}

const base: Values = {
  enabled: true,
  pollInterval: '6h',
  scope: 'labeled',
  cleanup: false,
  rollbackOnFailure: false,
  rollbackTimeout: '120s',
};

describe('AutoUpdatePanel validation', () => {
  it('accepts a poll interval at or above the 1m floor', async () => {
    expect(await validate({ ...base, pollInterval: '1m' })).toBeUndefined();
    expect(await validate({ ...base, pollInterval: '6h' })).toBeUndefined();
    expect(
      await validate({ ...base, pollInterval: '1h30m' })
    ).toBeUndefined();
  });

  it('rejects a poll interval below the 1m floor', async () => {
    expect(await validate({ ...base, pollInterval: '1s' })).toContain(
      'Poll interval must be at least 1m'
    );
    expect(await validate({ ...base, pollInterval: '59s' })).toContain(
      'Poll interval must be at least 1m'
    );
    expect(await validate({ ...base, pollInterval: '500ms' })).toContain(
      'Poll interval must be at least 1m'
    );
  });

  it('rejects a malformed duration', async () => {
    expect(await validate({ ...base, pollInterval: 'soon' })).toContain(
      'Must be a valid duration (e.g. 6h, 30m, 1h30m)'
    );
  });

  it('rejects an empty poll interval', async () => {
    expect(await validate({ ...base, pollInterval: '' })).toContain(
      'Poll interval is required'
    );
  });

  it('accepts a rollback timeout at or above the 10s floor', async () => {
    expect(await validate({ ...base, rollbackTimeout: '10s' })).toBeUndefined();
    expect(
      await validate({ ...base, rollbackTimeout: '120s' })
    ).toBeUndefined();
    expect(await validate({ ...base, rollbackTimeout: '2m' })).toBeUndefined();
  });

  it('rejects a rollback timeout below the 10s floor', async () => {
    expect(await validate({ ...base, rollbackTimeout: '1ms' })).toContain(
      'Rollback timeout must be at least 10s'
    );
    expect(await validate({ ...base, rollbackTimeout: '9s' })).toContain(
      'Rollback timeout must be at least 10s'
    );
  });

  it('rejects an empty rollback timeout', async () => {
    expect(await validate({ ...base, rollbackTimeout: '' })).toContain(
      'Rollback timeout is required'
    );
  });

  it('rejects a malformed rollback timeout', async () => {
    expect(await validate({ ...base, rollbackTimeout: 'soon' })).toContain(
      'Must be a valid duration (e.g. 120s, 2m)'
    );
  });
});
