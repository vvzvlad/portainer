import { ReactNode } from 'react';

import { IconProps } from '@@/Icon';

export type Value = number | string | boolean;

export interface BoxSelectorOption<T extends Value> extends IconProps {
  readonly id: string;
  readonly label: string;
  readonly description?: ReactNode;
  readonly value: T;
  readonly disabled?: boolean | (() => boolean);
  readonly tooltip?: () => string;
  readonly hide?: boolean;
  readonly iconType?: 'raw' | 'badge' | 'logo';
  readonly iconClass?: string;
}
