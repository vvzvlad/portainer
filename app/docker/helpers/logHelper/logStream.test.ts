import { createLogStreamProcessor } from './logStream';
import { resetLineIdSequence } from './lineId';

const encoder = new TextEncoder();

function bytes(s: string): Uint8Array {
  return encoder.encode(s);
}

// Build a real Docker multiplexed-stream frame: 1 byte stream-type, 3 zero
// bytes, a 4-byte big-endian payload length, then the payload bytes.
function frame(payload: Uint8Array | string, streamType = 1): Uint8Array {
  const body = typeof payload === 'string' ? bytes(payload) : payload;
  const out = new Uint8Array(8 + body.length);
  out[0] = streamType;
  const len = body.length;
  out[4] = (len >>> 24) & 0xff;
  out[5] = (len >>> 16) & 0xff;
  out[6] = (len >>> 8) & 0xff;
  out[7] = len & 0xff;
  out.set(body, 8);
  return out;
}

function concat(...parts: Uint8Array[]): Uint8Array {
  const total = parts.reduce((n, p) => n + p.length, 0);
  const out = new Uint8Array(total);
  let off = 0;
  parts.forEach((p) => {
    out.set(p, off);
    off += p.length;
  });
  return out;
}

beforeEach(() => {
  resetLineIdSequence();
});

describe('createLogStreamProcessor (non-TTY, byte-level frame demux)', () => {
  it('emits only completed lines and carries the partial remainder forward', () => {
    const proc = createLogStreamProcessor({ stripHeaders: true });

    // first chunk: one full frame then a frame whose line has no newline yet
    const first = proc.push(concat(frame('hello\n'), frame('wor')));
    expect(first.map((l) => l.line)).toEqual(['hello']);

    // completing the partial line + a new one (across two frames)
    const second = proc.push(concat(frame('ld\n'), frame('bye\n')));
    expect(second.map((l) => l.line)).toEqual(['world', 'bye']);
  });

  it('returns nothing until a newline arrives, then flushes the trailing partial', () => {
    const proc = createLogStreamProcessor({ stripHeaders: true });

    expect(proc.push(frame('partial')).map((l) => l.line)).toEqual([]);
    // stream ended without a trailing newline -> flush yields the remainder
    expect(proc.flush().map((l) => l.line)).toEqual(['partial']);
  });

  it('strips the frame header from every framed line', () => {
    const proc = createLogStreamProcessor({ stripHeaders: true });
    const lines = proc.push(concat(frame('line-1\n'), frame('line-2\n')));
    expect(lines.map((l) => l.line)).toEqual(['line-1', 'line-2']);
  });

  // F1: a frame whose payload length low-byte is 0x0a (10). A text-level
  // demuxer that splits on '\n' before stripping headers would land inside the
  // header and desync, mis-rendering "123456789\n" down to "9".
  it('handles a frame whose length low-byte is 0x0a (payload of exactly 10 bytes)', () => {
    const proc = createLogStreamProcessor({ stripHeaders: true });
    const lines = proc.push(frame('123456789\n')); // payload length = 10 = 0x0a
    expect(lines.map((l) => l.line)).toEqual(['123456789']);
  });

  // F2 (boundary): a chunk boundary falling between the 8-byte header and its
  // payload.
  it('handles a chunk split between the header and the payload', () => {
    const proc = createLogStreamProcessor({ stripHeaders: true });
    const full = frame('abc\n');
    expect(proc.push(full.subarray(0, 8)).map((l) => l.line)).toEqual([]);
    expect(proc.push(full.subarray(8)).map((l) => l.line)).toEqual(['abc']);
  });

  // F2: a length field with a byte >= 0x80 (200 = 0xC8), split across chunks.
  // UTF-8-decoding the header would mangle 0xC8 and slip the offset.
  it('handles a large frame (length byte >= 0x80) split across chunks', () => {
    const proc = createLogStreamProcessor({ stripHeaders: true });
    const payload = `${'x'.repeat(199)}\n`; // 200 bytes -> length byte 0xC8
    const full = frame(payload);
    expect(full[7]).toBe(0xc8);
    expect(proc.push(full.subarray(0, 5)).map((l) => l.line)).toEqual([]);
    const rest = proc.push(full.subarray(5));
    expect(rest.map((l) => l.line)).toEqual(['x'.repeat(199)]);
  });

  // F2 (multibyte): a multibyte UTF-8 char split across two chunks must be
  // reassembled before decoding (not turned into U+FFFD).
  it('reassembles a multibyte UTF-8 char split across two chunks', () => {
    const proc = createLogStreamProcessor({ stripHeaders: true });
    const full = frame('aéb\n'); // é = 0xC3 0xA9
    const eIdx = full.indexOf(0xc3);
    expect(proc.push(full.subarray(0, eIdx + 1)).map((l) => l.line)).toEqual([]);
    const rest = proc.push(full.subarray(eIdx + 1));
    expect(rest.map((l) => l.line)).toEqual(['aéb']);
  });

  it('drops a truncated final frame on flush instead of emitting garbage', () => {
    const proc = createLogStreamProcessor({ stripHeaders: true });
    const full = frame('never\n');
    // only the header + 2 payload bytes arrive, then the stream ends
    expect(proc.push(full.subarray(0, 10)).map((l) => l.line)).toEqual([]);
    expect(proc.flush().map((l) => l.line)).toEqual([]);
  });

  // F3: a framed line terminated by \r\n must have its trailing CR stripped.
  it('strips the trailing CR from a \\r\\n line ending (framed)', () => {
    const proc = createLogStreamProcessor({ stripHeaders: true });
    const lines = proc.push(frame('with-crlf\r\n'));
    expect(lines.map((l) => l.line)).toEqual(['with-crlf']);
  });
});

describe('createLogStreamProcessor (TTY, no headers)', () => {
  it('passes lines through unchanged', () => {
    const proc = createLogStreamProcessor({ stripHeaders: false });
    const lines = proc.push(bytes('plain line\n'));
    expect(lines.map((l) => l.line)).toEqual(['plain line']);
  });

  it('carries a partial line across chunks and flushes the remainder', () => {
    const proc = createLogStreamProcessor({ stripHeaders: false });
    expect(proc.push(bytes('par')).map((l) => l.line)).toEqual([]);
    expect(proc.push(bytes('tial\n')).map((l) => l.line)).toEqual(['partial']);
    expect(proc.push(bytes('tail')).map((l) => l.line)).toEqual([]);
    expect(proc.flush().map((l) => l.line)).toEqual(['tail']);
  });

  // F3: a \r\n line ending must have its trailing CR stripped (non-TTY path).
  it('strips the trailing CR from a \\r\\n line ending', () => {
    const proc = createLogStreamProcessor({ stripHeaders: false });
    const lines = proc.push(bytes('with-crlf\r\n'));
    expect(lines.map((l) => l.line)).toEqual(['with-crlf']);
  });

  // F3: a remainder carrying a trailing CR (no terminating LF) must have the CR
  // stripped on flush. Split across two chunks to exercise the buffered path.
  it('strips a trailing CR on flush when the stream ends without a newline', () => {
    const proc = createLogStreamProcessor({ stripHeaders: false });
    expect(proc.push(bytes('hel')).map((l) => l.line)).toEqual([]);
    expect(proc.push(bytes('lo\r')).map((l) => l.line)).toEqual([]);
    expect(proc.flush().map((l) => l.line)).toEqual(['hello']);
  });
});

describe('timestamps (always requested, optionally displayed)', () => {
  const ts1 = '2024-01-01T00:00:00.000000001Z';
  const ts2 = '2024-01-01T00:00:00.000000002Z';

  it('strips the RFC3339 prefix when timestamps are hidden', () => {
    const proc = createLogStreamProcessor({
      stripHeaders: false,
      withTimestamps: false,
      streamHasTimestamps: true,
    });
    const lines = proc.push(bytes(`${ts1} hello\n`));
    expect(lines.map((l) => l.line)).toEqual(['hello']);
    expect(proc.getLastTimestamp()).toBe(ts1);
  });

  it('keeps the RFC3339 prefix when timestamps are shown', () => {
    const proc = createLogStreamProcessor({
      stripHeaders: false,
      withTimestamps: true,
      streamHasTimestamps: true,
    });
    const lines = proc.push(bytes(`${ts1} hello\n`));
    expect(lines.map((l) => l.line)).toEqual([`${ts1} hello`]);
  });

  it('drops lines re-delivered at/before the reconnect resume point', () => {
    const proc = createLogStreamProcessor({
      stripHeaders: false,
      withTimestamps: false,
      streamHasTimestamps: true,
      skipUntilTimestamp: ts1,
    });
    // ts1 is the inclusive boundary Docker re-delivers -> dropped; ts2 is new
    const lines = proc.push(bytes(`${ts1} old\n${ts2} new\n`));
    expect(lines.map((l) => l.line)).toEqual(['new']);
  });

  // F4: the real reconnect shape — the redelivered boundary line arrives in its
  // OWN chunk (the whole batch is dropped, dropTo === lines.length), so the
  // `skipping` flag must stay on and the NEXT chunk must keep dropping dups up
  // to the resume point before emitting the first genuinely new line.
  it('keeps dropping reconnect dups when the redelivered line arrives in its own chunk', () => {
    const tsA = '2024-01-01T00:00:00.000000001Z';
    const tsB = '2024-01-01T00:00:00.000000002Z';
    const tsC = '2024-01-01T00:00:00.000000003Z';
    const tsD = '2024-01-01T00:00:00.000000004Z';
    const proc = createLogStreamProcessor({
      stripHeaders: false,
      withTimestamps: false,
      streamHasTimestamps: true,
      skipUntilTimestamp: tsB,
    });

    // (1) only a redelivered boundary line (<= resume point) -> fully consumed
    // by dedup; nothing emitted and skipping stays on.
    expect(proc.push(bytes(`${tsA} dup-1\n`)).map((l) => l.line)).toEqual([]);

    // (2) another dup + the first new line: because skipping was preserved the
    // dup is still dropped, and only the new line is returned.
    expect(
      proc.push(bytes(`${tsB} dup-2\n${tsC} new\n`)).map((l) => l.line)
    ).toEqual(['new']);

    // (3) a later line passes through untouched (no skipping anymore).
    expect(proc.push(bytes(`${tsD} after\n`)).map((l) => l.line)).toEqual(['after']);
  });
});

describe('stable line ids (append model)', () => {
  it('assigns unique, strictly increasing ids across successive chunks', () => {
    const proc = createLogStreamProcessor({ stripHeaders: false });
    const a = proc.push(bytes('a\nb\n'));
    const b = proc.push(bytes('c\n'));

    const ids = [...a, ...b].map((l) => l.id);
    expect(new Set(ids).size).toBe(ids.length); // all unique
    const sorted = [...ids].sort((x, y) => x - y);
    expect(ids).toEqual(sorted); // monotonically increasing in arrival order
  });
});
