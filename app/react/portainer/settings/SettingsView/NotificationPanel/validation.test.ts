import { validation } from './validation';
import { Values } from './types';

function validate(values: Values) {
  return validation()
    .validate(values, { abortEarly: false })
    .then(() => undefined)
    .catch((err: { errors: string[] }) => err.errors);
}

const empty: Values = { updateWebhookUrl: '', healWebhookUrl: '' };

describe('NotificationPanel validation', () => {
  it('accepts both webhook URLs empty (disabled)', async () => {
    expect(await validate(empty)).toBeUndefined();
  });

  it('accepts a valid http(s) URL, including the placeholder, in each field', async () => {
    expect(
      await validate({ ...empty, updateWebhookUrl: 'https://example.com/notify' })
    ).toBeUndefined();
    expect(
      await validate({ ...empty, healWebhookUrl: 'http://example.com/notify' })
    ).toBeUndefined();
    expect(
      await validate({
        updateWebhookUrl: 'https://example.com/update?msg={{message}}',
        healWebhookUrl: 'https://example.com/heal?msg={{message}}',
      })
    ).toBeUndefined();
  });

  it('rejects a non-http(s) or malformed auto-update URL', async () => {
    expect(
      await validate({ ...empty, updateWebhookUrl: 'example.com/notify' })
    ).toContain('Must be a valid http(s) URL');
    expect(
      await validate({ ...empty, updateWebhookUrl: 'ftp://example.com' })
    ).toContain('Must be a valid http(s) URL');
    expect(
      await validate({ ...empty, updateWebhookUrl: 'not a url' })
    ).toContain('Must be a valid http(s) URL');
  });

  it('rejects a non-http(s) or malformed auto-heal URL', async () => {
    expect(
      await validate({ ...empty, healWebhookUrl: 'example.com/notify' })
    ).toContain('Must be a valid http(s) URL');
    expect(
      await validate({ ...empty, healWebhookUrl: 'ftp://example.com' })
    ).toContain('Must be a valid http(s) URL');
    expect(await validate({ ...empty, healWebhookUrl: 'not a url' })).toContain(
      'Must be a valid http(s) URL'
    );
  });
});
