import React from 'react';
const cx = (...c) => c.filter(Boolean).join(' ');
const useFid = (id) => { const g = React.useId().replace(/:/g, ''); return id || 'kv-' + g; };
function useControlled(value, defaultValue, onChange) {
  const [inner, setInner] = React.useState(defaultValue);
  const v = value !== undefined ? value : inner;
  const set = (n) => { if (value === undefined) setInner(n); onChange && onChange(n); };
  return [v, set];
}

export function Switch({ id, label, hint, className, checked, defaultChecked = false, onCheckedChange, disabled, name }) {
  const fid = useFid(id);
  const [v, set] = useControlled(checked, defaultChecked, onCheckedChange);
  const state = v ? 'checked' : 'unchecked';
  return (
    <div className={cx('kv-choice', 'kv-choice--switch', className)}>
      <button type="button" role="switch" id={fid} name={name} className="kv-switch" data-state={state} aria-checked={!!v} disabled={disabled}
        onClick={() => set(!v)} style={disabled ? { opacity: 0.5, cursor: 'not-allowed' } : undefined}>
        <span className="kv-switch-thumb" data-state={state} />
      </button>
      {label && <label htmlFor={fid}><span>{label}</span>{hint && <small>{hint}</small>}</label>}
    </div>
  );
}
