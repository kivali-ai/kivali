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

export function TextField({ id, label, hint, error, multiline = false, rows = 4, className, ...rest }) {
  const fid = useFid(id);
  const common = { id: fid, className: cx('kv-input', multiline && 'kv-input--multi', className),
    'aria-invalid': error ? true : undefined, 'aria-describedby': (hint || error) ? fid + '-msg' : undefined, ...rest };
  return <Field id={fid} label={label} hint={hint} error={error}>{multiline ? <textarea rows={rows} {...common} /> : <input {...common} />}</Field>;
}
