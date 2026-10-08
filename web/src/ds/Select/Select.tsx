import type { SelectHTMLAttributes } from 'react';
import { cx } from '../cx';
import { Field, useFieldId } from '../Field';
import { Icon } from '../Icon/Icon';

export interface SelectProps extends SelectHTMLAttributes<HTMLSelectElement> {
  /** Optional only so a caller can pass `aria-label` instead (AutoRelease does). */
  label?: string;
  hint?: string;
  error?: string;
  options: { value: string; label: string }[];
}

/**
 * A labeled native select, styled to match `TextField`.
 *
 * - `options` is a list of `{value, label}`; `label`, `hint` and `error` work as in `TextField`.
 * - Use it for short fixed lists (model, effort, resolution). For more than about 12 options or search, a combobox will come later.
 */
export function Select({ id, label, hint, error, options = [], className, ...rest }: SelectProps) {
  const fid = useFieldId(id);
  return (
    <Field id={fid} label={label} hint={hint} error={error}>
      <span className="kv-select">
        <select
          id={fid}
          className={cx('kv-input', className)}
          aria-invalid={error ? true : undefined}
          aria-describedby={hint || error ? fid + '-msg' : undefined}
          {...rest}
        >
          {options.map((o) => (
            <option key={o.value} value={o.value}>
              {o.label}
            </option>
          ))}
        </select>
        <Icon name="chevron-down" />
      </span>
    </Field>
  );
}
