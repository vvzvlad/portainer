import { formatLogs } from './formatLogs';
import { resetLineIdSequence } from './lineId';

beforeEach(() => {
  resetLineIdSequence();
});

describe('formatLogs stable ids', () => {
  it('gives every line a unique id within a call', () => {
    const lines = formatLogs('one\ntwo\nthree\n');
    const ids = lines.map((l) => l.id);
    expect(new Set(ids).size).toBe(ids.length);
  });

  it('continues the id sequence across calls (so appended chunks never collide)', () => {
    const first = formatLogs('a\nb\n');
    const second = formatLogs('c\nd\n');
    const allIds = [...first, ...second].map((l) => l.id);
    expect(new Set(allIds).size).toBe(allIds.length);
    // every id in the second batch is greater than every id in the first
    const maxFirst = Math.max(...first.map((l) => l.id));
    expect(Math.min(...second.map((l) => l.id))).toBeGreaterThan(maxFirst);
  });

  it('preserves the parsed text content', () => {
    const lines = formatLogs('hello\nworld\n');
    expect(lines.map((l) => l.line)).toEqual(['hello', 'world']);
  });
});
