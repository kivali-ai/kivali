import type { InputHTMLAttributes, TextareaHTMLAttributes } from 'react';
import { cx } from '../cx';
import { Field, useFieldId } from '../Field';

export interface TextFieldProps extends InputHTMLAttributes<HTMLInputElement> {
  label: string;
  hint?: string;
  error?: string;
  multiline?: boolean;
  rows?: number;
}

/**
 * A labeled text input, or a text area with `multiline`.
 *
 * - Always pass `label`; add `hint` for one line of guidance, or `error` to show a danger message with an icon (it replaces the hint and sets `aria-invalid`).
 * - `multiline` renders a resizable textarea with `rows` (default 4). Use it for briefs, replies and assignment descriptions.
 * - Placeholders are a real example of valid input, never a repeat of the label.
 * - Other native input props (`name`, `value`, `onChange`, `required`) pass through.
 */
export function TextField({ id, label, hint, error, multiline = false, rows = 4, className, ...rest }: TextFieldProps) {
  const fid = useFieldId(id);
  const common = {
    id: fid,
    className: cx('kv-input', multiline && 'kv-input--multi', className),
    'aria-invalid': error ? true : undefined,
    'aria-describedby': hint || error ? fid + '-msg' : undefined,
  };
  return (
    <Field id={fid} label={label} hint={hint} error={error}>
      {multiline ? (
        <textarea rows={rows} {...common} {...(rest as unknown as TextareaHTMLAttributes<HTMLTextAreaElement>)} />
      ) : (
        <input {...common} {...rest} />
      )}
    </Field>
  );
}
