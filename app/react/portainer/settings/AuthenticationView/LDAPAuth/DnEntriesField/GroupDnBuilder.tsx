import { useEffect, useState } from 'react';

import { DnEntriesField } from './DnEntriesField';
import { GroupNameField } from './GroupNameField';
import { DnEntry, parseDN, buildDN } from './ldap-dn-utils';

interface Props {
  value: string;
  onChange: (index: number, dn: string) => void;
  suffix: string;
  index: number;
  onRemoveClick?: (index: number) => void;
}

export function GroupDnBuilder({
  value,
  onChange,
  suffix,
  index,
  onRemoveClick,
}: Props) {
  const [groupName, setGroupName] = useState(() =>
    parseGroupName(value, suffix)
  );
  const [entries, setEntries] = useState<DnEntry[]>(() =>
    parseDN(parsePath(value, suffix), suffix)
  );

  useEffect(() => {
    const groupName = parseGroupName(value, suffix);
    const entries = parseDN(parsePath(value, suffix), suffix);
    setGroupName(groupName);
    setEntries(entries);
    const dn = buildGroupDN(groupName, entries, suffix);
    if (dn !== value) {
      onChange(index, dn);
    }
  }, [index, onChange, suffix, value]);

  return (
    <>
      <GroupNameField
        id={`group-name-input-${index}`}
        value={groupName}
        onChange={(newGroupName) => {
          setGroupName(newGroupName);
          onChange(index, buildGroupDN(newGroupName, entries, suffix));
        }}
        onRemoveClick={onRemoveClick ? () => onRemoveClick(index) : undefined}
      />
      <DnEntriesField
        value={entries}
        onChange={(entries: DnEntry[]) => {
          setEntries(entries);
          if (groupName) {
            onChange(index, buildGroupDN(groupName, entries, suffix));
          }
        }}
        label="Path to group"
      />
    </>
  );
}

export function parseGroupName(value: string, suffix: string): string {
  if (!value || value === suffix) return '';
  const [groupNamePart] = value.split(/,(.+)/);
  return groupNamePart.replace(/^cn=/i, '');
}

function parsePath(value: string, suffix: string): string {
  if (!value || value === suffix) return suffix;
  const [, rest] = value.split(/,(.+)/);
  return rest || suffix;
}

export function buildGroupDN(
  groupName: string,
  entries: DnEntry[],
  suffix: string
): string {
  if (!groupName) {
    return suffix;
  }

  const groupNameEntry = `cn=${groupName}`;
  const path = buildDN(entries, suffix);
  const pathPart = path && path !== suffix ? path : suffix;
  return pathPart ? `${groupNameEntry},${pathPart}` : groupNameEntry;
}
