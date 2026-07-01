import { useEffect, useMemo, useRef, useState } from 'react';
import clsx from 'clsx';
import { saveAs } from 'file-saver';
import DateTimeRangePicker from '@wojtekmaj/react-datetimerange-picker';
import {
  Calendar,
  Check,
  Clock,
  Copy,
  Download,
  File,
  List,
  RefreshCw,
  Search,
  WrapText,
  X,
} from 'lucide-react';

import '@wojtekmaj/react-datetimerange-picker/dist/DateTimeRangePicker.css';
import 'react-calendar/dist/Calendar.css';
import 'react-datetime-picker/dist/DateTimePicker.css';

import { concatLogsToString } from '@/docker/helpers/logHelper';
import { FormattedLine } from '@/docker/helpers/logHelper/types';

import { useCopy } from '@@/buttons/CopyButton/useCopy';

import { StreamLogsFn } from './types';
import { useLogViewer } from './useLogViewer';
import { LogLine } from './LogLine';
import { ToggleButton } from './ToggleButton';

// The maintainer's mockup palette. This is a deliberately dark, self-contained
// design (log viewers are conventionally dark), so — unlike the rest of the app —
// this one component carries its colours inline instead of theme tokens. Only
// fonts/sizes are pulled back to the project scale (his one explicit ask).
const C = {
  card: '#0c0c0d',
  cardBorder: '#1e1f22',
  row: '#161719',
  rowBorder: '#202124',
  field: '#202124',
  fieldBorder: '#2c2e32',
  logBg: '#0b0b0c',
  text: '#e6e9ea',
  strong: '#f2f4f5',
  muted: '#9aa1a8',
  faint: '#6a7178',
  logText: '#c8d0ca',
  iconBox: '#1e2023',
  iconBoxBorder: '#2a2c30',
  downloadBg: '#2a2c30',
  downloadBorder: '#383b40',
  danger: '#e5484d',
};

interface Props {
  /** Opens the (optionally following) log stream — see StreamLogsFn. */
  streamLogs: StreamLogsFn;
  /**
   * Identifies the streamed resource (e.g. container id). `streamLogs` closes
   * over it, so it is forwarded to the hook to restart the stream on a switch.
   */
  resourceId: string;
  /** Non-TTY container: Docker multiplexes stdout/stderr with frame headers. */
  hasFrameHeaders: boolean;
  /** Used for the download filename (`<resourceName>_logs.txt`). */
  resourceName: string;
}

function filterLogs(logs: FormattedLine[], search: string): FormattedLine[] {
  const term = search.toLowerCase();
  return logs.filter((log) => log.line.toLowerCase().includes(term));
}

export function LogViewer({
  streamLogs,
  resourceId,
  hasFrameHeaders,
  resourceName,
}: Props) {
  // Presentation state (the hook owns only the line buffer + network lifecycle).
  const [search, setSearch] = useState('');
  const [filterResults, setFilterResults] = useState(false);
  const [lineNumbers, setLineNumbers] = useState(true);
  const [timestamps, setTimestamps] = useState(false);
  const [wrapLines, setWrapLines] = useState(true);
  const [lineCount, setLineCount] = useState(100);
  const [since, setSince] = useState<Date | null>(null);
  const [until, setUntil] = useState<Date | null>(null);
  const [reloadNonce, setReloadNonce] = useState(0);

  // The viewer live-follows by default and only switches to a bounded, static
  // snapshot when the datetime-range picker sets an upper bound. Deriving this
  // (rather than exposing a follow/pause toggle) keeps the toolbar to the three
  // toggles in the reference design: Line numbers / Timestamp / Wrap lines.
  const autoRefresh = !until;

  const scrollRef = useRef<HTMLDivElement>(null);
  // Whether the view is pinned to the bottom (so live lines keep it tailing).
  const stickToBottomRef = useRef(true);

  const { logs, error } = useLogViewer({
    streamLogs,
    resourceId,
    stripHeaders: hasFrameHeaders,
    autoRefresh,
    withTimestamps: timestamps,
    lineCount,
    since,
    until,
    reloadNonce,
  });

  const displayedLogs = useMemo(
    () => (filterResults && search ? filterLogs(logs, search) : logs),
    [logs, filterResults, search]
  );

  const logsAsString = useMemo(
    () => concatLogsToString(displayedLogs),
    [displayedLogs]
  );

  // Copy uses the project's secure-context-safe clipboard wrapper (rendered with
  // the mockup's own button styling below).
  const { handleCopy } = useCopy(logsAsString);

  // Keep the newest line in view while tailing, but only if the user has not
  // scrolled up to read history (so an active read is never yanked to the bottom).
  useEffect(() => {
    const el = scrollRef.current;
    if (el && stickToBottomRef.current) {
      el.scrollTop = el.scrollHeight;
    }
  }, [displayedLogs]);

  function onScroll() {
    const el = scrollRef.current;
    if (!el) {
      return;
    }
    const distanceFromBottom =
      el.scrollHeight - el.scrollTop - el.clientHeight;
    stickToBottomRef.current = distanceFromBottom < 24;
  }

  function downloadLogs() {
    saveAs(new Blob([logsAsString]), `${resourceName}_logs.txt`);
  }

  const hasLogs = displayedLogs.length > 0;

  return (
    // Only the maintainer's card is rendered here; Portainer's page provides the
    // breadcrumb and page title around it.
    <div className="w-full">
      <div
        style={{
          display: 'flex',
          flexDirection: 'column',
          background: C.card,
          border: `1px solid ${C.cardBorder}`,
          borderRadius: 12,
          overflow: 'hidden',
        }}
      >
        {/* Header row: file icon + "Logs" left, search / filter / copy / download right */}
        <div
          style={{
            display: 'flex',
            alignItems: 'center',
            flexWrap: 'wrap',
            gap: 14,
            padding: '13px 16px',
            background: C.row,
            borderBottom: `1px solid ${C.rowBorder}`,
          }}
        >
          <div style={{ display: 'flex', alignItems: 'center', gap: 11 }}>
            <span
              style={{
                width: 34,
                height: 34,
                borderRadius: 9,
                background: C.iconBox,
                border: `1px solid ${C.iconBoxBorder}`,
                display: 'flex',
                alignItems: 'center',
                justifyContent: 'center',
                color: '#c7cdd2',
              }}
            >
              <File size={17} />
            </span>
            <span className="text-sm font-semibold" style={{ color: C.strong }}>
              Logs
            </span>
            <button
              type="button"
              onClick={() => setReloadNonce((n) => n + 1)}
              title="Reload logs"
              aria-label="Reload logs"
              data-cy="log-viewer-reload"
              style={{
                display: 'flex',
                alignItems: 'center',
                background: 'none',
                border: 'none',
                cursor: 'pointer',
                color: C.muted,
              }}
            >
              <RefreshCw size={15} />
            </button>
          </div>

          <div style={{ flex: '1 1 auto' }} />

          {/* Search box */}
          <div style={{ position: 'relative', width: 230, maxWidth: '100%' }}>
            <Search
              size={14}
              style={{
                position: 'absolute',
                left: 12,
                top: '50%',
                transform: 'translateY(-50%)',
                color: C.faint,
              }}
            />
            <input
              id="log-viewer-search"
              type="text"
              value={search}
              placeholder="Search..."
              onChange={(e) => setSearch(e.target.value)}
              data-cy="log-viewer-search"
              className="text-xs"
              style={{
                width: '100%',
                height: 36,
                background: C.field,
                border: `1px solid ${C.fieldBorder}`,
                borderRadius: 8,
                padding: '0 32px 0 34px',
                color: C.text,
                outline: 'none',
                boxSizing: 'border-box',
              }}
            />
            {search && (
              <button
                type="button"
                onClick={() => setSearch('')}
                aria-label="Clear search"
                style={{
                  position: 'absolute',
                  right: 10,
                  top: '50%',
                  transform: 'translateY(-50%)',
                  display: 'flex',
                  background: 'none',
                  border: 'none',
                  cursor: 'pointer',
                  color: C.faint,
                }}
              >
                <X size={14} />
              </button>
            )}
          </div>

          {/* Filter search results — custom checkbox in the mockup's style */}
          <button
            type="button"
            onClick={() => setFilterResults((v) => !v)}
            aria-pressed={filterResults}
            data-cy="log-viewer-filter-results"
            className="text-xs"
            style={{
              display: 'flex',
              alignItems: 'center',
              gap: 9,
              background: 'none',
              border: 'none',
              cursor: 'pointer',
              padding: 0,
              color: C.text,
            }}
          >
            <span
              style={{
                width: 18,
                height: 18,
                borderRadius: 5,
                flex: '0 0 auto',
                display: 'flex',
                alignItems: 'center',
                justifyContent: 'center',
                border: `1.5px solid ${filterResults ? '#e6e9ea' : '#3a3d42'}`,
                background: filterResults ? '#e6e9ea' : 'transparent',
              }}
            >
              {filterResults && (
                <Check size={12} strokeWidth={3.5} color="#0a0a0b" />
              )}
            </span>
            <span style={{ whiteSpace: 'nowrap', fontWeight: 500 }}>
              Filter search results
            </span>
          </button>

          {/* Copy */}
          <button
            type="button"
            onClick={handleCopy}
            data-cy="log-viewer-copy"
            className="text-xs"
            style={{
              display: 'flex',
              alignItems: 'center',
              gap: 7,
              height: 36,
              padding: '0 13px',
              background: C.field,
              border: `1px solid ${C.fieldBorder}`,
              borderRadius: 8,
              color: C.text,
              fontWeight: 500,
              cursor: 'pointer',
              whiteSpace: 'nowrap',
            }}
          >
            <Copy size={15} />
            Copy
          </button>

          {/* Download logs */}
          <button
            type="button"
            onClick={downloadLogs}
            disabled={!hasLogs}
            data-cy="log-viewer-download"
            className="text-xs"
            style={{
              display: 'flex',
              alignItems: 'center',
              gap: 7,
              height: 36,
              padding: '0 13px',
              background: C.downloadBg,
              border: `1px solid ${C.downloadBorder}`,
              borderRadius: 8,
              color: C.strong,
              fontWeight: 500,
              cursor: hasLogs ? 'pointer' : 'not-allowed',
              opacity: hasLogs ? 1 : 0.5,
              whiteSpace: 'nowrap',
            }}
          >
            <Download size={15} />
            Download logs
          </button>
        </div>

        {/* Toolbar row: datetime range / lines / toggles */}
        <div
          style={{
            display: 'flex',
            alignItems: 'center',
            flexWrap: 'wrap',
            gap: 11,
            padding: '11px 16px',
            background: C.row,
            borderBottom: `1px solid ${C.rowBorder}`,
          }}
        >
          {/* One combined "from – to" datetime range control (with time). The
              functional @wojtekmaj picker is dropped into a frame styled like the
              mockup's range control; clearing it (null) drops the time window. */}
          <div
            data-cy="log-viewer-datetime-range"
            className="text-xs"
            style={{
              display: 'flex',
              alignItems: 'center',
              height: 36,
              background: C.field,
              border: `1px solid ${C.fieldBorder}`,
              borderRadius: 8,
              padding: '0 6px 0 12px',
              color: C.text,
            }}
          >
            <DateTimeRangePicker
              // Strip the picker's own wrapper border so only the frame shows.
              className="h-full [&>div]:border-0"
              format="y-MM-dd HH:mm"
              value={since || until ? [since, until] : null}
              onChange={(value) => {
                if (Array.isArray(value)) {
                  setSince(value[0]);
                  setUntil(value[1]);
                  return;
                }
                setSince(value);
                setUntil(null);
              }}
              calendarIcon={<Calendar size={14} />}
              clearIcon={<X size={14} />}
            />
          </div>

          {/* Lines (historical-backfill tail request) */}
          <div
            style={{
              display: 'flex',
              alignItems: 'center',
              height: 36,
              background: C.field,
              border: `1px solid ${C.fieldBorder}`,
              borderRadius: 8,
              overflow: 'hidden',
            }}
          >
            <label
              htmlFor="log-viewer-lines"
              className="text-xs !mb-0"
              style={{
                display: 'flex',
                alignItems: 'center',
                height: '100%',
                padding: '0 12px',
                color: '#8a9198',
                borderRight: `1px solid ${C.fieldBorder}`,
                fontWeight: 400,
              }}
            >
              Lines
            </label>
            <input
              id="log-viewer-lines"
              type="number"
              min={1}
              value={lineCount}
              placeholder="Lines..."
              onChange={(e) => {
                // Clearing a number input yields NaN (which React rejects as a
                // value); fall back to 0, which the hook treats as "use the
                // default tail".
                const n = e.target.valueAsNumber;
                setLineCount(Number.isNaN(n) ? 0 : n);
              }}
              data-cy="log-viewer-lines"
              className="text-xs"
              style={{
                width: 64,
                height: '100%',
                background: 'none',
                border: 'none',
                outline: 'none',
                color: C.text,
                padding: '0 12px',
              }}
            />
          </div>

          {/* Display toggles */}
          <div
            style={{
              display: 'flex',
              alignItems: 'center',
              flexWrap: 'wrap',
              gap: 7,
            }}
          >
            <ToggleButton
              active={lineNumbers}
              onChange={setLineNumbers}
              label="Line numbers"
              icon={List}
              title="Toggle line numbers"
              data-cy="log-viewer-line-numbers"
            />
            <ToggleButton
              active={timestamps}
              onChange={setTimestamps}
              label="Timestamp"
              icon={Clock}
              title="Toggle timestamps"
              data-cy="log-viewer-timestamps"
            />
            <ToggleButton
              active={wrapLines}
              onChange={setWrapLines}
              label="Wrap lines"
              icon={WrapText}
              title="Toggle line wrapping"
              data-cy="log-viewer-wrap"
            />
          </div>
        </div>

        {error && (
          <div
            className="text-xs"
            style={{ padding: '8px 16px', background: C.logBg, color: C.danger }}
          >
            {error}
          </div>
        )}

        {/* Log body — project monospace (log_viewer font-mono); the dark palette
            is intentional. */}
        <div
          ref={scrollRef}
          onScroll={onScroll}
          className={clsx(
            'log_viewer font-mono',
            wrapLines ? 'whitespace-pre-wrap break-words' : 'whitespace-pre'
          )}
          style={{
            height: '60vh',
            background: C.logBg,
            color: C.logText,
            padding: '12px 4px 18px 0',
          }}
          data-cy="log-viewer-body"
        >
          {hasLogs ? (
            displayedLogs.map((line, index) => (
              <LogLine
                key={line.id}
                line={line}
                lineNumber={index + 1}
                showLineNumber={lineNumbers}
                search={search}
              />
            ))
          ) : (
            <div className="text-xs" style={{ padding: '8px 16px', color: C.muted }}>
              {search
                ? `No log line matching the '${search}' filter`
                : 'No logs available'}
            </div>
          )}
        </div>
      </div>
    </div>
  );
}
