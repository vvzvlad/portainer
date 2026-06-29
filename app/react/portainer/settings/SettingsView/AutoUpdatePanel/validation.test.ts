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
});
