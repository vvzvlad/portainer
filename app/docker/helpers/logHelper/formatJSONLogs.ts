import { without } from 'lodash';

import {
  FormattedLineContent,
  Span,
  JSONLogs,
  TIMESTAMP_LENGTH,
} from './types';
import {
  formatCaller,
  formatKeyValuePair,
  formatLevel,
  formatMessage,
  formatStackTrace,
  formatTime,
} from './formatters';

function removeKnownKeys(keys: string[]) {
  return without(keys, 'time', 'level', 'caller', 'message', 'stack_trace');
}

export function formatJSONLine(
  rawText: string,
  withTimestamps?: boolean
): FormattedLineContent[] {
  const spans: Span[] = [];
  const lines: FormattedLineContent[] = [];
  let line = '';

  const text = withTimestamps ? rawText.substring(TIMESTAMP_LENGTH) : rawText;

  const parsed: unknown = JSON.parse(text);
  // Only treat the line as a structured JSON log when it actually parses to a
  // plain object. A line serialized as a bare JSON string (e.g. `"hello"`)
  // parses to a string, whose `Object.keys` is `['0','1',...]` — which used to
  // render as `0=h 1=e ...`. Fall back to the plain-text path in that case.
  if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) {
    const plain = rawText;
    return [{ line: plain, spans: [{ text: plain }] }];
  }
  const json = parsed as JSONLogs;
  const { time, level, caller, message, stack_trace: stackTrace } = json;
  const keys = removeKnownKeys(Object.keys(json));

  if (withTimestamps) {
    const timestamp = rawText.substring(0, TIMESTAMP_LENGTH);
    spans.push({ text: timestamp });
    line += `${timestamp} `;
  }
  line += formatTime(time, spans, line);
  line += formatLevel(level, spans, line);
  line += formatCaller(caller, spans, line);
  line += formatMessage(message, spans, line, !!keys.length);

  keys.forEach((key, idx) => {
    line += formatKeyValuePair(
      key,
      json[key],
      spans,
      line,
      idx === keys.length - 1
    );
  });

  lines.push({ line, spans });
  formatStackTrace(stackTrace, lines);

  return lines;
}
