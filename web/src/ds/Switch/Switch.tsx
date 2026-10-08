import { useId, useState } from 'react';
import * as RSwitch from '@radix-ui/react-switch';
import { cx } from '../cx';

export interface SwitchProps {
  id?: string;
  label: string;
  hint?: string;
  className?: string;
  checked?: boolean;
  defaultChecked?: boolean;
  onCheckedChange?(v: boolean): void;
  disabled?: boolean;
  name?: string;
}

/**
 * An on/off setting that takes effect immediately (enabling a skill, auto-release). Built on Radix Switch.
 *
 * - On, the track is ink and the thumb turns signal yellow: yellow on ink, as the brand rule asks.
 * - Pass `checked` and `onCheckedChange`; `label` is required and `hint` is optional.
 * - If the change needs a save button, use `Checkbox` instead.
 */
export function Switch({ id, label, hint, className, checked, defaultChecked = false, onCheckedChange, disabled, name }: SwitchProps) {
  const generated = 'kv-' + useId().replace(/:/g, '');
  const fid = id ?? generated;
  const [inner, setInner] = useState(defaultChecked);
  const v = checked !== undefined ? checked : inner;
  return (
    <div className={cx('kv-choice', 'kv-choice--switch', className)}>
      <RSwitch.Root
        id={fid}
        name={name}
        className="kv-switch"
        checked={v}
        disabled={disabled}
        onCheckedChange={(n) => {
          if (checked === undefined) setInner(n);
          onCheckedChange?.(n);
        }}
        style={disabled ? { opacity: 0.5, cursor: 'not-allowed' } : undefined}
      >
        <RSwitch.Thumb className="kv-switch-thumb" />
      </RSwitch.Root>
      {label && (
        <label htmlFor={fid}>
          <span>{label}</span>
          {hint && <small>{hint}</small>}
        </label>
      )}
    </div>
  );
}
