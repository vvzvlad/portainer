import { Plus } from 'lucide-react';

import { Select, Input } from '@@/form-components/Input';
import { Widget, WidgetBody } from '@@/Widget';
import { Button } from '@@/buttons';
import { useInputList } from '@@/form-components/InputList/useInputList';
import { InputListActionButtons } from '@@/form-components/InputList/ActionButtons';

import { DnEntry } from './ldap-dn-utils';

const typeOptions = [
  { label: 'OU Name', value: 'ou' },
  { label: 'Folder Name', value: 'cn' },
] as const;

export function DnEntriesField({
  value,
  onChange,
  label = 'DN entries',
}: {
  value: DnEntry[];
  onChange: (entries: DnEntry[]) => void;
  label?: string;
}) {
  const {
    handleMoveUp,
    handleMoveDown,
    handleRemoveItem,
    handleAdd,
    handleChangeItem,
  } = useInputList({
    value,
    onChange,
    itemBuilder: () => ({ type: 'ou' as const, value: '' }),
    movable: true,
  });

  return (
    <>
      <div>
        <label className="control-label text-left">{label}</label>
        <Button
          onClick={handleAdd}
          size="xsmall"
          color="light"
          icon={Plus}
          className="ml-2 !border-0"
          data-cy="ldap-dn-builder-add-button"
        >
          add another entry
        </Button>
      </div>
      {value.length > 0 && (
        <Widget className="mt-1">
          <WidgetBody>
            <div className="flex flex-col gap-y-5">
              {value.map((entry, index) => (
                <div key={index} className="flex">
                  <DnEntryItem
                    item={entry}
                    onChange={(newEntry) => handleChangeItem(index, newEntry)}
                  />
                  <InputListActionButtons
                    index={index}
                    count={value.length}
                    movable
                    onMoveUp={() => handleMoveUp(index)}
                    onMoveDown={() => handleMoveDown(index)}
                    onDelete={() => handleRemoveItem(index, entry)}
                    data-cy="ldap-dn-builder"
                  />
                </div>
              ))}
            </div>
          </WidgetBody>
        </Widget>
      )}
    </>
  );
}

interface DnEntryItemProps {
  item: DnEntry;
  onChange: (value: DnEntry) => void;
  disabled?: boolean;
  readOnly?: boolean;
}

function DnEntryItem({ item, onChange, disabled, readOnly }: DnEntryItemProps) {
  return (
    <div className="flex w-full gap-2">
      <div className="w-1/3">
        <Select
          options={typeOptions}
          value={item.type}
          onChange={(e) =>
            onChange({ ...item, type: e.target.value as 'ou' | 'cn' })
          }
          disabled={disabled}
          data-cy="ldap-dn-builder-select"
        />
      </div>
      <div className="w-5/12">
        <Input
          value={item.value}
          onChange={(e) => onChange({ ...item, value: e.target.value })}
          disabled={disabled}
          readOnly={readOnly}
          data-cy="ldap-dn-builder-input"
        />
      </div>
    </div>
  );
}
