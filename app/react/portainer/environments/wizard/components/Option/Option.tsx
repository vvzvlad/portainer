import clsx from 'clsx';
import { ComponentType } from 'react';

import styles from './Option.module.css';

export interface SelectorItemType {
  icon: string | ComponentType<{ selected?: boolean; className?: string }>;
  title: string;
  description: string;
}

interface Props extends SelectorItemType {
  active?: boolean;
  onClick?(): void;
}

export function Option({
  icon,
  active,
  description,
  title,
  onClick = () => {},
}: Props) {
  const IconComponent = icon;
  return (
    <button
      className={clsx(styles.root, styles.feature, 'border-0', {
        [styles.active]: active,
      })}
      type="button"
      onClick={onClick}
    >
      <div className="mt-2 flex items-end justify-center text-center">
        <IconComponent selected={active} className={styles.iconComponent} />
      </div>

      <div className="mt-3 flex flex-col text-center">
        <h3>{title}</h3>
        <h5>{description}</h5>
      </div>
    </button>
  );
}
