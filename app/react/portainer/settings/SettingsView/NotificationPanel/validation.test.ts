import { validation } from './validation';
import { Values } from './types';

function validate(values: Values) {
  return validation()
    .validate(values, { abortEarly: false })
    .then(() => undefined)
    .catch((err: { errors: string[] }) => err.errors);
}

describe('NotificationPanel validation', () => {
  it('accepts an empty webhook URL (disabled)', async () => {
    expect(await validate({ webhookUrl: '' })).toBeUndefined();
  });

  it('accepts a valid http(s) URL, including the placeholder', async () => {
    expect(
      await validate({ webhookUrl: 'https://example.com/notify' })
    ).toBeUndefined();
    expect(
      await validate({ webhookUrl: 'http://example.com/notify' })
    ).toBeUndefined();
    expect(
      await validate({
        webhookUrl: 'https://example.com/notify?msg={{message}}',
      })
    ).toBeUndefined();
  });

  it('rejects a non-http(s) or malformed URL', async () => {
    expect(await validate({ webhookUrl: 'example.com/notify' })).toContain(
      'Must be a valid http(s) URL'
    );
    expect(await validate({ webhookUrl: 'ftp://example.com' })).toContain(
      'Must be a valid http(s) URL'
    );
    expect(await validate({ webhookUrl: 'not a url' })).toContain(
      'Must be a valid http(s) URL'
    );
  });
});
