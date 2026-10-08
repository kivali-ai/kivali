import { useId, useState } from 'react';
import * as RCheckbox from '@radix-ui/react-checkbox';
import { cx } from '../cx';
import { Icon } from '../Icon/Icon';

export interface CheckboxProps {
  id?: string;
  label: string;
  hint?: string;
  className?: string;
  checked?: boolean | 'indeterminate';
  defaultChecked?: boolean;
  onCheckedChange?(v: boolean | 'indeterminate'): void;
  disabled?: boolean;
  name?: string;
  /** Accessible name when `label` is empty (a row-selection checkbox). */
  ariaLabel?: string;
}

/**
 * A checkbox with its label, built on Radix Checkbox for keyboard and screen-reader support.
 *
 * - Use it for choices that apply when a form is submitted, and in table rows for bulk selection. For settings that take effect immediately, use `Switch`.
 * - Pass `checked` and `onCheckedChange`, or `defaultChecked`. `checked="indeterminate"` shows a dash (a partial "select all").
 * - `label` is required; `hint` adds a muted second line.
 */
export function Checkbox({ id, label, hint, className, checked, defaultChecked = false, onCheckedChange, disabled, name, ariaLabel }: CheckboxProps) {
  const generated = 'kv-' + useId().replace(/:/g, '');
  const fid = id ?? generated;
  const [inner, setInner] = useState<boolean | 'indeterminate'>(defaultChecked);
  const v = checked !== undefined ? checked : inner;
  const state = v === 'indeterminate' ? 'indeterminate' : v ? 'checked' : 'unchecked';
  return (
    <div className={cx('kv-choice', className)}>
      <RCheckbox.Root
        id={fid}
        name={name}
        aria-label={ariaLabel}
        className="kv-checkbox"
        checked={v}
        disabled={disabled}
        onCheckedChange={(n) => {
          if (checked === undefined) setInner(n);
          onCheckedChange?.(n);
        }}
        style={disabled ? { opacity: 0.5, cursor: 'not-allowed' } : undefined}
      >
        <RCheckbox.Indicator className="kv-checkbox-ind">
          <Icon name={state === 'indeterminate' ? 'minus' : 'check'} size={14} />
        </RCheckbox.Indicator>
      </RCheckbox.Root>
      {label && (
        <label htmlFor={fid}>
          <span>{label}</span>
          {hint && <small>{hint}</small>}
        </label>
      )}
    </div>
  );
}
