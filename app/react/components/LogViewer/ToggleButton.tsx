import { ComponentType, CSSProperties } from 'react';

import { AutomationTestingProps } from '@/types';

interface Props extends AutomationTestingProps {
  active: boolean;
  onChange: (active: boolean) => void;
  label: string;
  /** A lucide-style icon component (rendered at a small fixed size). */
  icon: ComponentType<{ size?: number | string }>;
  title?: string;
}

// Toggle pill matching the maintainer's log-viewer mockup: rounded, icon + label,
// active = lighter border/background/text, inactive = transparent + muted colors.
// Font family/size are left to the project (text-xs + inherited UI font) instead
// of the mockup's hardcoded Inter/13px.
function toggleStyle(active: boolean): CSSProperties {
  return {
    display: 'flex',
    alignItems: 'center',
    gap: 7,
    height: 36,
    padding: '0 13px',
    borderRadius: 8,
    fontWeight: 500,
    cursor: 'pointer',
    whiteSpace: 'nowrap',
    border: `1px solid ${active ? '#6a6f76' : '#3a3d42'}`,
    background: active ? 'rgba(255,255,255,0.04)' : 'transparent',
    color: active ? '#f2f4f5' : '#9aa1a8',
  };
}

/**
 * An independent on/off toggle rendered as a real <button> with `aria-pressed`.
 * The viewer's Line numbers / Timestamp / Wrap lines controls are independent
 * toggles (not a single-select segmented control), so each is its own
 * ToggleButton.
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
    <button
      type="button"
      className="text-xs"
      style={toggleStyle(active)}
      title={title}
      aria-pressed={active}
      onClick={() => onChange(!active)}
      data-cy={dataCy}
    >
      <Icon size={14} />
      {label}
    </button>
  );
}
