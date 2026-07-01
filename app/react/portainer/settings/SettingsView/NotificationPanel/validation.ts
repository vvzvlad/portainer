import { SchemaOf, object, string } from 'yup';

import { Values } from './types';

// isHttpUrl reports whether a value parses as an http(s) URL. The webhook URL
// may carry the "{{message}}" placeholder, so we only check the scheme rather
// than a strict format, mirroring the backend validation.
function isHttpUrl(value: string): boolean {
  try {
    const url = new URL(value);
    return url.protocol === 'http:' || url.protocol === 'https:';
  } catch {
    return false;
  }
}

export function validation(): SchemaOf<Values> {
  return object({
    // Optional field: an empty URL disables the webhook.
    webhookUrl: string()
      .default('')
      .test('valid-webhook-url', 'Must be a valid http(s) URL', (value) => {
        if (!value) {
          return true;
        }

        return isHttpUrl(value);
      }),
  });
}
