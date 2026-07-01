import { ComponentType } from 'react';
import clsx from 'clsx';

import { AutomationTestingProps } from '@/types';

import { Button } from '@@/buttons';

interface Props extends AutomationTestingProps {
  active: boolean;
  onChange: (active: boolean) => void;
  label: string;
  /** A lucide-style icon component (rendered at a small fixed size). */
  icon: ComponentType<{ size?: number | string }>;
  title?: string;
}

/**
 * An independent on/off toggle rendered as the project's themed Button with
 * `aria-pressed`. The viewer's Line numbers / Timestamp / Wrap lines controls
 * are independent toggles (not a single-select segmented control), so each is
 * its own ToggleButton. Active uses the primary (pressed) colour, inactive the
 * neutral default; both come from the theme so the pill adapts to light/dark/
 * high-contrast like the rest of the app.
 */
export function ToggleButton({
  active,
  onChange,
  label,
  icon: Icon,
  title,
  'data-cy': dataCy,
}: Props) {
  return (
    <Button
      color={active ? 'primary' : 'default'}
      size="small"
      icon={Icon}
      title={title}
      aria-pressed={active}
      onClick={() => onChange(!active)}
      className={clsx('!m-0', !active && 'th-dark:!text-gray-4')}
      data-cy={dataCy}
    >
      {label}
    </Button>
  );
}
