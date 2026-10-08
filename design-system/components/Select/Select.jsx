import React from 'react';
import { Icon } from '../Icon/Icon.jsx';
const cx = (...c) => c.filter(Boolean).join(' ');
const useFid = (id) => { const g = React.useId().replace(/:/g, ''); return id || 'kv-' + g; };
function Field({ id, label, hint, error, children }) {
  return (
    <div className={cx('kv-field', error && 'is-invalid')}>
      {label && <label className="kv-field-label" htmlFor={id}>{label}</label>}
      {children}
      {error ? <p className="kv-field-error" id={id + '-msg'}><Icon name="triangle-alert" size={14} />{error}</p>
        : hint ? <p className="kv-field-hint" id={id + '-msg'}>{hint}</p> : null}
    </div>
  );
}

export function Select({ id, label, hint, error, options = [], className, ...rest }) {
  const fid = useFid(id);
  return (
    <Field id={fid} label={label} hint={hint} error={error}>
      <span className="kv-select">
        <select id={fid} className={cx('kv-input', className)} aria-invalid={error ? true : undefined}
          aria-describedby={(hint || error) ? fid + '-msg' : undefined} {...rest}>
          {options.map((o) => <option key={o.value} value={o.value}>{o.label}</option>)}
        </select>
        <Icon name="chevron-down" />
      </span>
    </Field>
  );
}
