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
  checkInterval: '30s',
  scope: 'labeled',
};

describe('AutoHealPanel validation', () => {
  it('accepts a check interval at or above the 1s floor', async () => {
    expect(await validate({ ...base, checkInterval: '1s' })).toBeUndefined();
    expect(await validate({ ...base, checkInterval: '30s' })).toBeUndefined();
    expect(await validate({ ...base, checkInterval: '2m' })).toBeUndefined();
  });

  it('rejects a check interval below the 1s floor (backend 400s on these)', async () => {
    expect(await validate({ ...base, checkInterval: '1ms' })).toContain(
      'Check interval must be at least 1s'
    );
    expect(await validate({ ...base, checkInterval: '500ms' })).toContain(
      'Check interval must be at least 1s'
    );
    expect(await validate({ ...base, checkInterval: '0s' })).toContain(
      'Check interval must be at least 1s'
    );
  });

  it('rejects a malformed duration', async () => {
    expect(await validate({ ...base, checkInterval: 'soon' })).toContain(
      'Must be a valid duration (e.g. 30s, 1m, 2h)'
    );
  });

  it('rejects an empty check interval', async () => {
    expect(await validate({ ...base, checkInterval: '' })).toContain(
      'Check interval is required'
    );
  });
});
