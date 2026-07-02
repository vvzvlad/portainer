import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { isoDateFromTimestamp } from '@/portainer/filters/filters';
import { StackFileVersionInfo } from '@/react/common/stacks/types';

import { StackVersionSelector } from './StackVersionSelector';

function createInfo(
  overrides: Partial<StackFileVersionInfo> = {}
): StackFileVersionInfo {
  return {
    Version: 1,
    CreatedAt: 1751464320, // fixed unix timestamp (seconds)
    CreatedBy: 'admin',
    Note: '',
    ...overrides,
  };
}

it('should render nothing when there are no versions', () => {
  const { container } = render(
    <StackVersionSelector versions={[]} onChange={vi.fn()} />
  );

  expect(container).toBeEmptyDOMElement();
});

it('should render a rich label with version, date and author for a single version', () => {
  const info = createInfo({ Version: 3, CreatedBy: 'alice' });

  render(
    <StackVersionSelector
      versions={[3]}
      versionsInfo={[info]}
      onChange={vi.fn()}
    />
  );

  const expected = `v3 · ${isoDateFromTimestamp(info.CreatedAt)} · alice`;
  expect(screen.getByText(expected)).toBeInTheDocument();
});

it('should fall back to the bare version number when no metadata is available', () => {
  render(<StackVersionSelector versions={[7]} onChange={vi.fn()} />);

  expect(screen.getByText('v7')).toBeInTheDocument();
});

it('should render a select with rich labels when multiple versions exist', () => {
  const versionsInfo = [
    createInfo({ Version: 2, CreatedBy: 'bob' }),
    createInfo({ Version: 1, CreatedBy: 'alice' }),
  ];

  render(
    <StackVersionSelector
      versions={[2, 1]}
      versionsInfo={versionsInfo}
      onChange={vi.fn()}
    />
  );

  const select = screen.getByRole('combobox', { name: /version/i });
  expect(select).toBeInTheDocument();

  const options = screen.getAllByRole('option');
  expect(options).toHaveLength(2);
  expect(options[0]).toHaveTextContent(
    `v2 · ${isoDateFromTimestamp(versionsInfo[0].CreatedAt)} · bob`
  );
  expect(options[1]).toHaveTextContent(
    `v1 · ${isoDateFromTimestamp(versionsInfo[1].CreatedAt)} · alice`
  );
});

it('should call onChange with the selected version number', async () => {
  const user = userEvent.setup();
  const onChange = vi.fn();

  render(
    <StackVersionSelector
      versions={[3, 2, 1]}
      versionsInfo={[
        createInfo({ Version: 3 }),
        createInfo({ Version: 2 }),
        createInfo({ Version: 1 }),
      ]}
      onChange={onChange}
    />
  );

  await user.selectOptions(
    screen.getByRole('combobox', { name: /version/i }),
    '2'
  );

  expect(onChange).toHaveBeenCalledWith(2);
});
