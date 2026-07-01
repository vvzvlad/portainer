// Build Docker's `since` param as a Unix timestamp with a nanosecond fraction
// ("<seconds>.<nanos>") from an RFC3339Nano timestamp. Both the initial connect
// and reconnect use this one form so we never mix unix-seconds and RFC3339 —
// some proxies / pinned API versions accept them inconsistently. The fraction
// needs string precision (a JS number cannot hold nanoseconds), hence `since`
// is returned as a string.
//
// Docker emits a fixed-width RFC3339Nano prefix: "2006-01-02T15:04:05.000000000Z".
// We parse the second-precision prefix in UTC for the integer seconds, then take
// the fractional digits verbatim: short fractions (e.g. ".5") are right-padded
// to 9 digits, longer ones are truncated to 9 (nanosecond precision).
export function rfc3339ToUnixNanoSince(rfc3339: string): string {
  const seconds = Math.floor(
    Date.parse(`${rfc3339.substring(0, 19)}Z`) / 1000
  );
  const dot = rfc3339.indexOf('.');
  const nanos =
    dot >= 0
      ? rfc3339
          .substring(dot + 1)
          .replace(/[^0-9]/g, '')
          .padEnd(9, '0')
          .substring(0, 9)
      : '000000000';
  return `${seconds}.${nanos}`;
}
