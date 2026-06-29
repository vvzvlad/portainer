import { createLogStreamProcessor } from './logStream';
import { resetLineIdSequence } from './lineId';

// 8 arbitrary bytes standing in for Docker's multiplexed-stream frame header.
// `stripHeadersFunc` only ever drops 8 chars, so the content is irrelevant.
const H = 'HHHHHHHH';

beforeEach(() => {
  resetLineIdSequence();
});

describe('createLogStreamProcessor (non-TTY, stripHeaders)', () => {
  it('emits only completed lines and carries the partial remainder forward', () => {
    const proc = createLogStreamProcessor({ stripHeaders: true });

    // first chunk ends mid-line: only "hello" is complete
    const first = proc.push(`${H}hello\n${H}wor`);
    expect(first.map((l) => l.line)).toEqual(['hello']);

    // completing the partial line + a new one
    const second = proc.push(`ld\n${H}bye\n`);
    expect(second.map((l) => l.line)).toEqual(['world', 'bye']);
  });

  it('returns nothing until a newline arrives, then flushes the trailing partial', () => {
    const proc = createLogStreamProcessor({ stripHeaders: true });

    expect(proc.push(`${H}partial`)).toEqual([]);
    // stream ended without a trailing newline -> flush yields the remainder
    expect(proc.flush().map((l) => l.line)).toEqual(['partial']);
  });

  it('strips the 8-byte header from every framed line', () => {
    const proc = createLogStreamProcessor({ stripHeaders: true });
    const lines = proc.push(`${H}line-1\n${H}line-2\n`);
    expect(lines.map((l) => l.line)).toEqual(['line-1', 'line-2']);
  });
});

describe('createLogStreamProcessor (TTY, no headers)', () => {
  it('passes lines through unchanged', () => {
    const proc = createLogStreamProcessor({ stripHeaders: false });
    const lines = proc.push('plain line\n');
    expect(lines.map((l) => l.line)).toEqual(['plain line']);
  });
});

describe('stable line ids (append model)', () => {
  it('assigns unique, strictly increasing ids across successive chunks', () => {
    const proc = createLogStreamProcessor({ stripHeaders: false });
    const a = proc.push('a\nb\n');
    const b = proc.push('c\n');

    const ids = [...a, ...b].map((l) => l.id);
    expect(new Set(ids).size).toBe(ids.length); // all unique
    const sorted = [...ids].sort((x, y) => x - y);
    expect(ids).toEqual(sorted); // monotonically increasing in arrival order
  });
});
