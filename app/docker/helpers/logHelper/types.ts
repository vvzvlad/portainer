import { FontWeight } from '@xterm/xterm';

import { type TextColor } from './colors';

export type Token = string | number;

export type Level =
  | 'debug'
  | 'info'
  | 'warn'
  | 'error'
  | 'DBG'
  | 'INF'
  | 'WRN'
  | 'ERR';

export type JSONStackTrace = {
  func: string;
  line: string;
  source: string;
}[];

export type JSONLogs = {
  [k: string]: unknown;
  time: number;
  level: Level;
  caller: string;
  message: string;
  stack_trace?: JSONStackTrace;
};

export type Span = {
  fgColor?: TextColor;
  bgColor?: TextColor;
  text: string;
  fontWeight?: FontWeight;
};

// A rendered log line without its stable id. Internal formatters build these,
// and `formatLogs` assigns the `id` once, centrally, on the way out.
export type FormattedLineContent = {
  spans: Span[];
  line: string;
};

// A rendered log line with a stable, monotonically increasing id. The id lets
// the AngularJS template use `track by log.id`, so already-rendered rows are
// never re-bound (their text nodes are not rewritten) — which is what keeps a
// live text selection from collapsing while new lines stream in.
export type FormattedLine = FormattedLineContent & {
  id: number;
};

export const TIMESTAMP_LENGTH = 31; // 30 for timestamp + 1 for trailing space

export const Colors = {
  Grey: 'var(--text-log-viewer-color-json-grey)',
  Magenta: 'var(--text-log-viewer-color-json-magenta)',
  Yellow: 'var(--text-log-viewer-color-json-yellow)',
  Green: 'var(--text-log-viewer-color-json-green)',
  Red: 'var(--text-log-viewer-color-json-red)',
  Blue: 'var(--text-log-viewer-color-json-blue)',
};
