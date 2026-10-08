import React from 'react';
import { Icon } from '../Icon/Icon.jsx';
const cx = (...c) => c.filter(Boolean).join(' ');
const useFid = (id) => { const g = React.useId().replace(/:/g, ''); return id || 'kv-' + g; };
function useControlled(value, defaultValue, onChange) {
  const [inner, setInner] = React.useState(defaultValue);
  const v = value !== undefined ? value : inner;
  const set = (n) => { if (value === undefined) setInner(n); onChange && onChange(n); };
  return [v, set];
}

// Radix-style API (checked / defaultChecked / onCheckedChange); rendered without the Radix dependency.
export function Checkbox({ id, label, hint, className, checked, defaultChecked = false, onCheckedChange, disabled, name, ariaLabel }) {
  const fid = useFid(id);
  const [v, set] = useControlled(checked, defaultChecked, onCheckedChange);
  const state = v === 'indeterminate' ? 'indeterminate' : v ? 'checked' : 'unchecked';
  return (
    <div className={cx('kv-choice', className)}>
      <button type="button" role="checkbox" id={fid} name={name} aria-label={ariaLabel} className="kv-checkbox" data-state={state} disabled={disabled}
        aria-checked={v === 'indeterminate' ? 'mixed' : !!v} onClick={() => set(v === 'indeterminate' ? true : !v)}
        style={disabled ? { opacity: 0.5, cursor: 'not-allowed' } : undefined}>
        {state !== 'unchecked' && <span className="kv-checkbox-ind"><Icon name={state === 'indeterminate' ? 'minus' : 'check'} size={14} /></span>}
      </button>
      {label && <label htmlFor={fid}><span>{label}</span>{hint && <small>{hint}</small>}</label>}
    </div>
  );
}
