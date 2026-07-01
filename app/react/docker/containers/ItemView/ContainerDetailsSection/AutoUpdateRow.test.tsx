import { render, screen } from '@testing-library/react';

import { AutoUpdateRow } from './AutoUpdateRow';

function renderRow(labels?: Record<string, string>) {
  return render(
    <table>
      <tbody>
        <AutoUpdateRow labels={labels} />
      </tbody>
    </table>
  );
}

describe('AutoUpdateRow', () => {
  it('shows enabled when the primary label is true', () => {
    renderRow({ 'io.portainer.update.enable': 'true' });
    expect(screen.getByText('Enabled')).toBeInTheDocument();
  });

  it('shows enabled via the watchtower alias', () => {
    renderRow({ 'com.centurylinklabs.watchtower.enable': 'true' });
    expect(screen.getByText('Enabled')).toBeInTheDocument();
  });

  it.each(['TRUE', 'True', 'T', 't', '1'])(
    'parses the truthy value %s like strconv.ParseBool',
    (value) => {
      renderRow({ 'io.portainer.update.enable': value });
      expect(screen.getByText('Enabled')).toBeInTheDocument();
    }
  );

  it('treats a present-but-invalid value as opted out', () => {
    renderRow({ 'io.portainer.update.enable': 'soon' });
    expect(screen.getByText('Disabled (opted out)')).toBeInTheDocument();
  });

  it('shows opted out when the label is false', () => {
    renderRow({ 'io.portainer.update.enable': 'false' });
    expect(screen.getByText('Disabled (opted out)')).toBeInTheDocument();
  });

  it('shows the global-scope fallback when no label is set', () => {
    renderRow({});
    expect(
      screen.getByText('Not labeled (follows global scope)')
    ).toBeInTheDocument();
  });

  it('surfaces the monitor-only note (primary label)', () => {
    renderRow({
      'io.portainer.update.enable': 'true',
      'io.portainer.update.monitor-only': 'true',
    });
    expect(
      screen.getByText(/Monitor-only: updates are detected but never applied/i)
    ).toBeInTheDocument();
  });

  it('surfaces the monitor-only note via the watchtower alias', () => {
    renderRow({
      'com.centurylinklabs.watchtower.monitor-only': 'true',
    });
    expect(
      screen.getByText(/Monitor-only: updates are detected but never applied/i)
    ).toBeInTheDocument();
  });
});
