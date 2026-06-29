// Monotonically increasing, process-wide sequence used to give every rendered
// log line a stable id. Because it never resets during a session, lines parsed
// from later stream chunks always get ids greater than earlier ones, so the
// AngularJS `track by log.id` repeat treats already-rendered rows as unchanged
// and never rewrites their DOM text nodes (which is what was collapsing text
// selections on every poll). See logHelper/types.ts (FormattedLine).
let sequence = 0;

export function nextLineId() {
   
  return sequence++;
}

// Test-only helper to make id assignments deterministic between unit tests.
export function resetLineIdSequence() {
  sequence = 0;
}
