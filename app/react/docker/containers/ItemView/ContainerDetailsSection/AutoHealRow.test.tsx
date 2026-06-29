import { render, screen } from '@testing-library/react';

import { AutoHealRow } from './AutoHealRow';

function renderRow(labels?: Record<string, string>) {
  return render(
    <table>
      <tbody>
        <AutoHealRow labels={labels} />
      </tbody>
    </table>
  );
}

describe('AutoHealRow', () => {
  it('shows enabled when the primary label is true', () => {
    renderRow({ 'io.portainer.autoheal.enable': 'true' });
    expect(screen.getByText('Enabled')).toBeInTheDocument();
  });

  it('shows enabled via the community alias', () => {
    renderRow({ autoheal: 'true' });
    expect(screen.getByText('Enabled')).toBeInTheDocument();
  });

  it.each(['TRUE', 'True', 'T', 't', '1'])(
    'parses the truthy value %s like strconv.ParseBool',
    (value) => {
      renderRow({ 'io.portainer.autoheal.enable': value });
      expect(screen.getByText('Enabled')).toBeInTheDocument();
    }
  );

  it('treats a present-but-invalid value as opted out', () => {
    renderRow({ 'io.portainer.autoheal.enable': 'yepp' });
    expect(screen.getByText('Disabled (opted out)')).toBeInTheDocument();
  });

  it('shows opted out when the label is false', () => {
    renderRow({ 'io.portainer.autoheal.enable': 'false' });
    expect(screen.getByText('Disabled (opted out)')).toBeInTheDocument();
  });

  it('shows the global-scope fallback when no label is set', () => {
    renderRow({});
    expect(
      screen.getByText('Not labeled (follows global scope)')
    ).toBeInTheDocument();
  });

  it('renders stop timeout and retries when present', () => {
    renderRow({
      'io.portainer.autoheal.enable': 'true',
      'io.portainer.autoheal.stop-timeout': '20',
      'io.portainer.autoheal.retries': '5',
    });
    expect(screen.getByText('Stop timeout: 20s')).toBeInTheDocument();
    expect(screen.getByText('Max retries: 5')).toBeInTheDocument();
  });
});
