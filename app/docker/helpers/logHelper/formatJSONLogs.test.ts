import { formatJSONLine } from './formatJSONLogs';

describe('formatJSONLine 0=d 1=e guard', () => {
  it('renders a bare JSON string as plain text (not index=char pairs)', () => {
    const raw = '"hello"';
    const lines = formatJSONLine(raw);
    expect(lines).toHaveLength(1);
    // faithfully passes the raw line through instead of iterating its chars
    expect(lines[0].line).toBe(raw);
    // would previously have produced "0=h 1=e 2=l ..." via Object.keys on a string
    expect(lines[0].line).not.toContain('0=h');
  });

  it('renders a bare JSON array as plain text', () => {
    const raw = '[1,2,3]';
    const lines = formatJSONLine(raw);
    expect(lines).toHaveLength(1);
    expect(lines[0].line).toBe(raw);
  });

  it('still parses a real JSON-object log line', () => {
    const lines = formatJSONLine('{"level":"info","message":"hi"}');
    expect(lines[0].line).toContain('hi');
    expect(lines[0].line).not.toContain('0=');
  });
});
