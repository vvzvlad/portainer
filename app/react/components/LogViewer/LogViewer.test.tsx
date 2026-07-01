import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { mockClipboard } from '@/react/test-utils/clipboard';

import { StreamLogsFn } from './types';
import { LogViewer } from './LogViewer';

// Capture what would be downloaded so we can assert Download acts on the
// currently displayed (optionally filtered) lines.
const saveAsMock = vi.fn();
vi.mock('file-saver', () => ({
  saveAs: (...args: unknown[]) => saveAsMock(...args),
}));

const encoder = new TextEncoder();

// jsdom's Blob has no async text(); read it via FileReader instead.
function readBlob(blob: Blob): Promise<string> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = () => resolve(reader.result as string);
    reader.onerror = () => reject(reader.error);
    reader.readAsText(blob);
  });
}

// A fake log source: synchronously emits the given raw text chunks, then keeps
// the stream "open" (resolves only when the caller aborts) so the viewer's live
// path never reconnects mid-test.
function makeStream(chunks: string[]): StreamLogsFn {
  return (_params, onChunk, signal) =>
    new Promise((resolve) => {
      chunks.forEach((chunk) => onChunk(encoder.encode(chunk)));
      signal.addEventListener('abort', () => resolve());
    });
}

function renderViewer(chunks: string[]) {
  return render(
    <LogViewer
      streamLogs={makeStream(chunks)}
      resourceId="container-1"
      // TTY (no multiplexed frame headers) so the fake chunks are plain text.
      hasFrameHeaders={false}
      resourceName="my-container"
    />
  );
}

describe('LogViewer', () => {
  beforeEach(() => {
    saveAsMock.mockClear();
  });

  it('renders streamed lines with the formatter colour spans', async () => {
    // ANSI red-coloured text -> the formatter emits a span with an fgColor.
    renderViewer(['[31mred line[0m\nplain line\n']);

    const redSpan = await screen.findByText('red line');
    expect(redSpan).toBeVisible();
    // colour comes straight from the existing formatter (rgb(...) inline style).
    expect(redSpan).toHaveStyle({ color: 'rgb(128, 0, 0)' });
    expect(await screen.findByText('plain line')).toBeVisible();
  });

  it('filters to matching lines only when "Filter search results" is checked', async () => {
    const user = userEvent.setup();
    renderViewer(['apple\nbanana\ncherry\n']);

    await screen.findByText('banana');

    await user.type(screen.getByTestId('log-viewer-search'), 'ban');

    // Not filtering yet: every line is still shown (matches are highlighted).
    expect(screen.getByText('apple')).toBeVisible();
    const mark = screen.getByText('ban', { selector: 'mark' });
    expect(mark).toBeVisible();

    await user.click(screen.getByTestId('log-viewer-filter-results'));

    // Now only the matching line remains.
    await waitFor(() => {
      expect(screen.queryByText('apple')).not.toBeInTheDocument();
    });
    expect(screen.getByText('ban', { selector: 'mark' })).toBeVisible();
    expect(screen.getByText('ana')).toBeVisible();
  });

  it('shows the line-number gutter only when the toggle is on', async () => {
    const user = userEvent.setup();
    const { container } = renderViewer(['one\ntwo\n']);

    await screen.findByText('one');
    expect(container.querySelector('.log-viewer-gutter')).toBeNull();

    await user.click(screen.getByTestId('log-viewer-line-numbers'));

    await waitFor(() => {
      expect(container.querySelector('.log-viewer-gutter')).not.toBeNull();
    });
    const gutters = container.querySelectorAll('.log-viewer-gutter');
    expect(gutters[0]).toHaveTextContent('1');
    expect(gutters[1]).toHaveTextContent('2');
  });

  it('toggles line wrapping via the Wrap lines button', async () => {
    const user = userEvent.setup();
    renderViewer(['line\n']);

    await screen.findByText('line');
    const body = screen.getByTestId('log-viewer-body');
    // default on
    expect(body).toHaveClass('whitespace-pre-wrap');

    await user.click(screen.getByTestId('log-viewer-wrap'));
    await waitFor(() => {
      expect(body).toHaveClass('whitespace-pre');
    });
  });

  it('downloads the currently displayed (filtered) lines', async () => {
    const user = userEvent.setup();
    renderViewer(['apple\nbanana\n']);
    await screen.findByText('banana');

    // Filter down to the matching line, then download.
    await user.type(screen.getByTestId('log-viewer-search'), 'banana');
    await user.click(screen.getByTestId('log-viewer-filter-results'));
    await waitFor(() => {
      expect(screen.queryByText('apple')).not.toBeInTheDocument();
    });

    await user.click(screen.getByTestId('log-viewer-download'));

    expect(saveAsMock).toHaveBeenCalledTimes(1);
    const [blob, filename] = saveAsMock.mock.calls[0];
    expect(filename).toBe('my-container_logs.txt');
    const text = await readBlob(blob as Blob);
    expect(text).toContain('banana');
    expect(text).not.toContain('apple');
  });

  it('copies the displayed lines to the clipboard', async () => {
    const { writeText } = mockClipboard();
    renderViewer(['hello world\n']);
    await screen.findByText('hello world');

    fireEvent.click(screen.getByText('Copy'));

    await waitFor(() => {
      expect(writeText).toHaveBeenCalledTimes(1);
    });
    expect(writeText.mock.calls[0][0]).toContain('hello world');
  });
});
