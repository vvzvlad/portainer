import { useEffect, useMemo, useRef, useState } from 'react';
import clsx from 'clsx';
import { saveAs } from 'file-saver';
import DateTimeRangePicker from '@wojtekmaj/react-datetimerange-picker';
import {
  Calendar,
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
import { Button } from '@@/buttons';
import { Icon } from '@@/Icon';
import { Input } from '@@/form-components/Input';
import { Checkbox } from '@@/form-components/Checkbox';

import { StreamLogsFn } from './types';
import { useLogViewer } from './useLogViewer';
import { LogLine } from './LogLine';
import { ToggleButton } from './ToggleButton';

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

  // Copy uses the project's secure-context-safe clipboard wrapper.
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
    const distanceFromBottom = el.scrollHeight - el.scrollTop - el.clientHeight;
    stickToBottomRef.current = distanceFromBottom < 24;
  }

  function downloadLogs() {
    saveAs(new Blob([logsAsString]), `${resourceName}_logs.txt`);
  }

  const hasLogs = displayedLogs.length > 0;

  return (
    // Fill the available page height so the viewer reaches down toward the
    // viewport bottom instead of leaving a dead white void below a fixed card.
    // The 150px offset accounts for Portainer's top nav + the page
    // header/breadcrumb rendered above this component.
    <div
      className="flex w-full flex-col"
      style={{ height: 'calc(100vh - 150px)' }}
    >
      {/* Themed card surface — mirrors the project's Card/Widget tokens so it
          adapts to light / dark / high-contrast like every other Portainer
          view. It is a flex column whose log body grows to fill the height. */}
      <div
        className={clsx(
          'flex min-h-0 flex-1 flex-col overflow-hidden rounded-xl border border-solid',
          'border-gray-5 bg-white',
          'th-dark:border-legacy-grey-3 th-dark:bg-gray-iron-11',
          'th-highcontrast:border-white th-highcontrast:bg-black'
        )}
      >
        {/* Header row: file icon + "Logs" left, search / filter / copy / download right */}
        <div
          className={clsx(
            'flex flex-wrap items-center gap-x-4 gap-y-2 px-4 py-3',
            'border-0 border-b border-solid border-gray-5 bg-gray-iron-2',
            'th-dark:border-legacy-grey-3 th-dark:bg-gray-iron-10',
            'th-highcontrast:border-white th-highcontrast:bg-gray-warm-10'
          )}
        >
          <div className="flex items-center gap-2">
            <Icon
              icon={File}
              size="md"
              className="text-gray-7 th-dark:text-gray-4 th-highcontrast:text-white"
            />
            <span className="text-sm font-semibold text-gray-9 th-dark:text-gray-2 th-highcontrast:text-white">
              Logs
            </span>
            <Button
              color="link"
              size="xsmall"
              icon={RefreshCw}
              onClick={() => setReloadNonce((n) => n + 1)}
              title="Reload logs"
              aria-label="Reload logs"
              className="!m-0 !p-0"
              data-cy="log-viewer-reload"
            />
          </div>

          <div className="flex-1" />

          {/* Search box */}
          <div className="relative w-56 max-w-full">
            <Search
              size={14}
              aria-hidden="true"
              className="absolute left-2.5 top-1/2 -translate-y-1/2 text-muted"
            />
            <Input
              id="log-viewer-search"
              type="text"
              value={search}
              placeholder="Search..."
              onChange={(e) => setSearch(e.target.value)}
              data-cy="log-viewer-search"
              className="!pl-8 !pr-8"
            />
            {search && (
              <button
                type="button"
                onClick={() => setSearch('')}
                aria-label="Clear search"
                className="absolute right-2 top-1/2 flex -translate-y-1/2 border-0 bg-transparent p-0 text-muted"
              >
                <X size={14} />
              </button>
            )}
          </div>

          {/* Filter search results — project themed checkbox */}
          <Checkbox
            id="log-viewer-filter-results"
            label="Filter search results"
            checked={filterResults}
            onChange={(e) => setFilterResults(e.target.checked)}
            bold={false}
            data-cy="log-viewer-filter-results"
          />

          {/* Copy */}
          <Button
            color="default"
            size="small"
            icon={Copy}
            onClick={handleCopy}
            className="!m-0"
            data-cy="log-viewer-copy"
          >
            Copy
          </Button>

          {/* Download logs */}
          <Button
            color="primary"
            size="small"
            icon={Download}
            onClick={downloadLogs}
            disabled={!hasLogs}
            className="!m-0"
            data-cy="log-viewer-download"
          >
            Download logs
          </Button>
        </div>

        {/* Toolbar row: datetime range / lines / toggles */}
        <div
          className={clsx(
            'flex flex-wrap items-center gap-2 px-4 py-2.5',
            'border-0 border-b border-solid border-gray-5 bg-gray-iron-2',
            'th-dark:border-legacy-grey-3 th-dark:bg-gray-iron-10',
            'th-highcontrast:border-white th-highcontrast:bg-gray-warm-10'
          )}
        >
          {/* One combined "from – to" datetime range control (with time). The
              functional @wojtekmaj picker is dropped into the project's themed
              form-control frame — the same approach as the app's DateRangePicker
              — so it reads correctly in both themes; clearing it (null) drops
              the time window. */}
          <div data-cy="log-viewer-datetime-range" className="text-xs">
            <DateTimeRangePicker
              // Wrap in a themed form-control and strip the picker's own inner
              // border so only the project frame shows.
              className="form-control h-9 [&>div]:border-0"
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
          <div className="flex items-center gap-2">
            <label
              htmlFor="log-viewer-lines"
              className="!mb-0 text-xs text-muted"
            >
              Lines
            </label>
            <Input
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
              className="!w-24"
            />
          </div>

          {/* Display toggles */}
          <div className="flex flex-wrap items-center gap-2">
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
          <div className="border-0 border-b border-solid border-gray-5 px-4 py-2 text-xs text-error-8 th-dark:border-legacy-grey-3 th-dark:text-error-6">
            {error}
          </div>
        )}

        {/* Log body — project monospace + themed `.log_viewer` background/text
            (from app.css, which follows the theme). Grows to fill the card so
            there is no dead space below; overrides `.log_viewer`'s height:100%
            with a flex fill. */}
        <div
          ref={scrollRef}
          onScroll={onScroll}
          className={clsx(
            'log_viewer font-mono',
            wrapLines ? 'whitespace-pre-wrap break-words' : 'whitespace-pre'
          )}
          style={{
            flex: '1 1 auto',
            minHeight: 0,
            height: 'auto',
            overflowY: 'auto',
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
            <div className="px-4 py-2 text-xs text-muted">
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
