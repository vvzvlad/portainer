import { useEffect, useMemo, useRef, useState } from 'react';
import clsx from 'clsx';
import { saveAs } from 'file-saver';
import DateTimeRangePicker from '@wojtekmaj/react-datetimerange-picker';
import {
  Calendar,
  Clock,
  Download,
  File,
  List,
  Maximize2,
  Minimize2,
  RefreshCw,
  WrapText,
  X,
} from 'lucide-react';

import '@wojtekmaj/react-datetimerange-picker/dist/DateTimeRangePicker.css';
import 'react-calendar/dist/Calendar.css';
import 'react-datetime-picker/dist/DateTimePicker.css';

import { concatLogsToString } from '@/docker/helpers/logHelper';
import { FormattedLine } from '@/docker/helpers/logHelper/types';

import { Widget, WidgetBody, WidgetTitle } from '@@/Widget';
import { Button, CopyButton } from '@@/buttons';
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
  const [autoRefresh, setAutoRefresh] = useState(true);
  const [lineCount, setLineCount] = useState(100);
  const [since, setSince] = useState<Date | null>(null);
  const [until, setUntil] = useState<Date | null>(null);
  const [fullscreen, setFullscreen] = useState(false);
  const [reloadNonce, setReloadNonce] = useState(0);

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
    <div
      className={clsx('row', {
        'fixed inset-0 z-50 m-0 overflow-auto bg-white p-4 th-dark:bg-black th-highcontrast:bg-black':
          fullscreen,
      })}
    >
      <div className="col-sm-12">
        <Widget>
          <WidgetTitle
            title={
              <span className="inline-flex items-center gap-2">
                Container logs
                <Button
                  type="button"
                  color="link"
                  size="small"
                  icon={RefreshCw}
                  className="!m-0 !p-0"
                  title="Reload logs"
                  aria-label="Reload logs"
                  onClick={() => setReloadNonce((n) => n + 1)}
                  data-cy="log-viewer-reload"
                />
              </span>
            }
          >
            <Button
              type="button"
              color="link"
              size="small"
              icon={fullscreen ? Minimize2 : Maximize2}
              title={fullscreen ? 'Exit fullscreen' : 'Fullscreen'}
              aria-label={fullscreen ? 'Exit fullscreen' : 'Fullscreen'}
              onClick={() => setFullscreen((v) => !v)}
              data-cy="log-viewer-fullscreen"
            />
          </WidgetTitle>

          <WidgetBody className="!px-0">
            {/* Inner "Logs" panel */}
            <div className="rounded border border-solid border-gray-5 th-dark:border-gray-9 th-highcontrast:border-gray-warm-9">
              {/* Panel header: title left, search + copy/download right */}
              <div className="flex flex-wrap items-center justify-between gap-x-4 gap-y-2 border-0 border-b border-solid border-gray-5 px-4 py-2 th-dark:border-gray-9 th-highcontrast:border-gray-warm-9">
                <span className="inline-flex items-center gap-1 font-medium">
                  <Icon icon={File} />
                  Logs
                </span>
                <div className="flex flex-wrap items-center justify-end gap-x-4 gap-y-2">
                  <div className="flex items-center gap-x-2">
                    <label
                      htmlFor="log-viewer-search"
                      className="control-label !w-auto whitespace-nowrap !p-0 text-left"
                    >
                      Search
                    </label>
                    <Input
                      id="log-viewer-search"
                      type="text"
                      className="!w-auto"
                      value={search}
                      placeholder="Filter..."
                      onChange={(e) => setSearch(e.target.value)}
                      data-cy="log-viewer-search"
                    />
                  </div>
                  <Checkbox
                    id="log-viewer-filter-results"
                    label="Filter search results"
                    bold={false}
                    checked={filterResults}
                    onChange={(e) => setFilterResults(e.target.checked)}
                    data-cy="log-viewer-filter-results"
                  />
                  <CopyButton
                    copyText={logsAsString}
                    color="default"
                    data-cy="log-viewer-copy"
                  >
                    Copy
                  </CopyButton>
                  <Button
                    type="button"
                    color="default"
                    size="small"
                    icon={Download}
                    disabled={!hasLogs}
                    onClick={downloadLogs}
                    data-cy="log-viewer-download"
                  >
                    Download logs
                  </Button>
                </div>
              </div>

              {/* Toolbar row: range picker / lines / toggles / auto refresh */}
              <div className="flex flex-wrap items-end gap-x-4 gap-y-2 px-4 py-2">
                <div className="flex items-center gap-x-2">
                  <span className="control-label !w-auto whitespace-nowrap !p-0 text-left">
                    Time range
                  </span>
                  {/* One combined "from – to" datetime range control (with
                      time), matching the reference. Clearing it (null) drops
                      the closed time window. */}
                  <div data-cy="log-viewer-datetime-range">
                    <DateTimeRangePicker
                      format="y-MM-dd HH:mm"
                      className="form-control [&>div]:border-0"
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
                      calendarIcon={<Calendar />}
                      clearIcon={<X />}
                    />
                  </div>
                </div>
                <div className="flex items-center gap-x-2">
                  <label
                    htmlFor="log-viewer-lines"
                    className="control-label !w-auto whitespace-nowrap !p-0 text-left"
                  >
                    Lines
                  </label>
                  <Input
                    id="log-viewer-lines"
                    type="number"
                    min={1}
                    className="!w-24"
                    value={lineCount}
                    placeholder="Lines..."
                    onChange={(e) => {
                      // Clearing a number input yields NaN (which React rejects
                      // as a value); fall back to 0, which the hook treats as
                      // "use the default tail".
                      const n = e.target.valueAsNumber;
                      setLineCount(Number.isNaN(n) ? 0 : n);
                    }}
                    data-cy="log-viewer-lines"
                  />
                </div>
                <div className="flex flex-wrap items-center gap-1">
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
                  <ToggleButton
                    active={autoRefresh}
                    onChange={setAutoRefresh}
                    label="Auto refresh"
                    icon={RefreshCw}
                    title="Toggle live tailing"
                    data-cy="log-viewer-auto-refresh"
                  />
                </div>
              </div>

              {error && (
                <div className="px-4 pb-2 text-sm text-danger">{error}</div>
              )}

              {/* Log body */}
              <div
                ref={scrollRef}
                onScroll={onScroll}
                className={clsx(
                  'log_viewer font-mono',
                  wrapLines ? 'whitespace-pre-wrap break-words' : 'whitespace-pre'
                )}
                style={{
                  height: fullscreen ? 'calc(100vh - 220px)' : '60vh',
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
                  <div className="px-4 py-2 text-muted">
                    {search
                      ? `No log line matching the '${search}' filter`
                      : 'No logs available'}
                  </div>
                )}
              </div>
            </div>
          </WidgetBody>
        </Widget>
      </div>
    </div>
  );
}
