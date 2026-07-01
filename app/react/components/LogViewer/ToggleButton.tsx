import { ComponentType, ReactNode } from 'react';

import { AutomationTestingProps } from '@/types';

import { Button } from '@@/buttons';

interface Props extends AutomationTestingProps {
  active: boolean;
  onChange: (active: boolean) => void;
  label: ReactNode;
  icon?: ReactNode | ComponentType<unknown>;
  title?: string;
}

/**
 * An independent on/off toggle rendered as a real <button> with `aria-pressed`.
 * The viewer's Line numbers / Timestamp / Wrap lines / Auto refresh controls are
 * independent toggles (not a single-select segmented control), so each is its
 * own ToggleButton.
 */
export function ToggleButton({
  active,
  onChange,
  label,
  icon,
  title,
  'data-cy': dataCy,
}: Props) {
  return (
    <Button
      type="button"
      size="small"
      color={active ? 'primary' : 'default'}
      icon={icon}
      title={title}
      aria-pressed={active}
      onClick={() => onChange(!active)}
      data-cy={dataCy}
    >
      {label}
    </Button>
  );
}
