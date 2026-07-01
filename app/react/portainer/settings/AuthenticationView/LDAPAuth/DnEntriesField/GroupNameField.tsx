import { Trash2 } from 'lucide-react';

import { Input } from '@@/form-components/Input';
import { Button } from '@@/buttons';

interface Props {
  id: string;
  value: string;
  onChange: (value: string) => void;
  disabled?: boolean;
  onRemoveClick?: () => void;
}

export function GroupNameField({
  id,
  value,
  onChange,
  disabled,
  onRemoveClick,
}: Props) {
  return (
    <div className="form-group">
      <label htmlFor={id} className="col-sm-4 control-label text-left">
        Group Name
      </label>
      <div className="col-sm-7 pl-0">
        <Input
          id={id}
          data-cy="group-name-input"
          value={value}
          onChange={(e) => onChange(e.target.value)}
          disabled={disabled}
          readOnly={disabled}
        />
      </div>
      {onRemoveClick && (
        <div className="col-sm-1">
          <Button
            type="button"
            color="danger"
            size="medium"
            className="vertical-center"
            onClick={onRemoveClick}
            disabled={disabled}
            icon={Trash2}
            data-cy="group-dn-remove-button"
            title="Remove Group"
            aria-label="Remove Group"
          />
        </div>
      )}
    </div>
  );
}
