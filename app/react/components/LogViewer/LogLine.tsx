import { Fragment, ReactNode } from 'react';

import { FormattedLine } from '@/docker/helpers/logHelper/types';

interface Props {
  line: FormattedLine;
  /** 1-based ordinal shown in the left gutter (when line numbers are on). */
  lineNumber?: number;
  showLineNumber: boolean;
  /** Case-insensitive substring to highlight (empty = no highlight). */
  search: string;
}

// Split a span's text on the search term (case-insensitive) and wrap matches in
// a <mark>. Highlighting is done per span, so a match that straddles two spans
// (e.g. a coloured boundary) is not highlighted — an accepted edge case: the
// formatter's spans are the colouring unit and re-tokenising them to highlight
// across boundaries would fight the existing pipeline.
function highlight(text: string, search: string): ReactNode {
  if (!search) {
    return text;
  }
  const lower = text.toLowerCase();
  const term = search.toLowerCase();
  const parts: ReactNode[] = [];
  let start = 0;
  for (;;) {
    const idx = lower.indexOf(term, start);
    if (idx === -1) {
      break;
    }
    if (idx > start) {
      parts.push(text.substring(start, idx));
    }
    parts.push(
      <mark key={idx} className="bg-warning-5 th-dark:bg-warning-9">
        {text.substring(idx, idx + term.length)}
      </mark>
    );
    start = idx + term.length;
  }
  if (parts.length === 0) {
    return text;
  }
  if (start < text.length) {
    parts.push(text.substring(start));
  }
  return parts.map((part, i) => <Fragment key={i}>{part}</Fragment>);
}

export function LogLine({ line, lineNumber, showLineNumber, search }: Props) {
  return (
    <div className="log-viewer-line flex">
      {showLineNumber && (
        <span
          // Left gutter (fixed width, right aligned). The muted colour comes
          // from theme tokens so it reads correctly in every theme. Kept as its
          // own class so the layout/tests can target it.
          className="log-viewer-gutter shrink-0 select-none pr-4 text-right text-gray-6 th-dark:text-gray-6 th-highcontrast:text-gray-5"
          style={{ width: '3.25rem' }}
          aria-hidden="true"
        >
          {lineNumber}
        </span>
      )}
      <span className="log-viewer-line-content min-w-0 flex-1">
        {line.spans.length === 0 ? (
          // keep empty lines visible (they still occupy a row)
          ' '
        ) : (
          line.spans.map((span, index) => (
            <span
              // spans have no stable id; index within a formatted line is stable
              key={index}
              style={{
                color: span.fgColor,
                backgroundColor: span.bgColor,
                fontWeight: span.fontWeight,
              }}
            >
              {highlight(span.text, search)}
            </span>
          ))
        )}
      </span>
    </div>
  );
}
