import { useCallback, useEffect, useState } from 'react';

import { DnEntriesField } from './DnEntriesField';
import { parseDN, buildDN, DnEntry } from './ldap-dn-utils';

interface Props {
  value: string | undefined;
  suffix: string;
  onChange: (dn: string) => void;
  label?: string;
}

export function DnBuilder({ value, suffix, onChange, label }: Props) {
  const [entries, setEntries] = useState<DnEntry[]>([]);

  const handleEntriesChange = useCallback(
    (newEntries: DnEntry[]) => {
      setEntries(newEntries);
      const dn = buildDN(newEntries, suffix);
      if (dn !== value) {
        onChange(dn);
      }
    },
    [suffix, value, onChange]
  );

  useEffect(() => {
    handleEntriesChange(parseDN(value, suffix));
  }, [value, suffix, handleEntriesChange]);

  return (
    <DnEntriesField value={entries} onChange={handleEntriesChange} label={label} />
  );
}
