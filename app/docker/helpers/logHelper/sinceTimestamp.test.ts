import { rfc3339ToUnixNanoSince } from './sinceTimestamp';

// Integer Unix seconds of the shared second-precision prefix, computed the same
// way the parser does (Date.parse of the "...:05Z" prefix, in UTC).
const unix = Math.floor(Date.parse('2024-01-01T00:00:00Z') / 1000);

describe('rfc3339ToUnixNanoSince', () => {
  it('keeps a standard fixed-width nanosecond fraction', () => {
    expect(rfc3339ToUnixNanoSince('2024-01-01T00:00:00.000000005Z')).toBe(
      `${unix}.000000005`
    );
  });

  it('emits a zero fraction when the timestamp has no fractional part', () => {
    expect(rfc3339ToUnixNanoSince('2024-01-01T00:00:00Z')).toBe(
      `${unix}.000000000`
    );
  });

  it('right-pads a sub-nanosecond-width fraction to 9 digits', () => {
    expect(rfc3339ToUnixNanoSince('2024-01-01T00:00:00.5Z')).toBe(
      `${unix}.500000000`
    );
  });

  it('truncates a fraction longer than 9 digits to nanosecond precision', () => {
    expect(rfc3339ToUnixNanoSince('2024-01-01T00:00:00.1234567890123Z')).toBe(
      `${unix}.123456789`
    );
  });
});
