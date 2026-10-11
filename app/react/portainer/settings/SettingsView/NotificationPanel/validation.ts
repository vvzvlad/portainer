import { SchemaOf, object, string } from 'yup';

import { Values } from './types';

// "${TELEGRAM_BOT_API_SERVER}" is replaced by the backend with a Bot API server
// address from its own environment; any http(s) address stands in for it here.
// eslint-disable-next-line no-template-curly-in-string
const BOT_API_SERVER_PLACEHOLDER = '${TELEGRAM_BOT_API_SERVER}';

// isHttpUrl reports whether a value parses as an http(s) URL. The webhook URL
// may carry the "{{message}}" placeholder, so we only check the scheme rather
// than a strict format, mirroring the backend validation.
function isHttpUrl(value: string): boolean {
  try {
    const url = new URL(
      value.split(BOT_API_SERVER_PLACEHOLDER).join('https://api.telegram.org')
    );
    return url.protocol === 'http:' || url.protocol === 'https:';
  } catch {
    return false;
  }
}

// optionalHttpUrl builds a schema for an independently-optional webhook URL: an
// empty value disables that mechanism's webhook, otherwise it must be an http(s)
// URL (the "{{message}}" placeholder is allowed).
function optionalHttpUrl() {
  return string()
    .default('')
    .test('valid-webhook-url', 'Must be a valid http(s) URL', (value) => {
      if (!value) {
        return true;
      }

      return isHttpUrl(value);
    });
}

export function validation(): SchemaOf<Values> {
  return object({
    // Each URL is independently optional: an empty URL disables that webhook.
    updateWebhookUrl: optionalHttpUrl(),
    healWebhookUrl: optionalHttpUrl(),
  });
}
