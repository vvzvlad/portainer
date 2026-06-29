import { PropsWithChildren, ReactNode } from 'react';
import { SchemaOf, string } from 'yup';

import { useStateWrapper } from '@/react/hooks/useStateWrapper';

import { FormControl } from '@@/form-components/FormControl';
import { Input } from '@@/form-components/Input';
import { TextTip } from '@@/Tip/TextTip';

interface Props {
  value: string;
  onChange(value: string): void;
  error?: string;
}

export function RefField({ value, onChange, error }: Props) {
  const [inputValue, updateInputValue] = useStateWrapper(value, onChange);
  const inputId = 'repository-reference-field';
  return (
    <Wrapper
      inputId={inputId}
      errors={error}
      tip={
        <>
          Specify a reference of the repository using the following syntax:
          branches with <code>refs/heads/branch_name</code> or tags with{' '}
          <code>refs/tags/tag_name</code>. If not specified, will use the
          default <code>HEAD</code> reference normally the <code>main</code>{' '}
          branch.
        </>
      }
    >
      <Input
        id={inputId}
        data-cy="repository-reference-input"
        value={inputValue}
        onChange={(e) => updateInputValue(e.target.value)}
        placeholder="refs/heads/main"
      />
    </Wrapper>
  );
}

function Wrapper({
  tip,
  children,
  errors,
  inputId,
}: PropsWithChildren<{ tip: ReactNode; errors?: string; inputId: string }>) {
  return (
    <div className="form-group">
      <span className="col-sm-12 mb-2">
        <TextTip color="blue">{tip}</TextTip>
      </span>
      <div className="col-sm-12">
        <FormControl
          label="Repository reference"
          inputId={inputId}
          required
          errors={errors}
        >
          {children}
        </FormControl>
      </div>
    </div>
  );
}

export function refFieldValidation(): SchemaOf<string> {
  return string().default('');
}
