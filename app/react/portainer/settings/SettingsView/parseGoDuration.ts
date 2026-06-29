// Matches Go's time.ParseDuration units (e.g. "6h", "30m", "1h30m").
export const durationPattern = /^(\d+(\.\d+)?(ns|us|µs|ms|s|m|h))+$/;

// Seconds per Go duration unit, used to evaluate a configured interval against a
// floor. Mirrors the units accepted by durationPattern.
const unitSeconds: Record<string, number> = {
  ns: 1e-9,
  us: 1e-6,
  µs: 1e-6,
  ms: 1e-3,
  s: 1,
  m: 60,
  h: 3600,
};

// parseGoDurationSeconds converts a Go-style duration (e.g. "1h30m") to seconds,
// or returns null when the string is not a well-formed duration.
export function parseGoDurationSeconds(value: string): number | null {
  if (!durationPattern.test(value)) {
    return null;
  }

  let total = 0;
  const componentPattern = /(\d+(?:\.\d+)?)(ns|us|µs|ms|s|m|h)/g;
  let match = componentPattern.exec(value);
  while (match !== null) {
    total += parseFloat(match[1]) * unitSeconds[match[2]];
    match = componentPattern.exec(value);
  }

  return total;
}
