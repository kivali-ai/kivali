import React from 'react';
const cx = (...c) => c.filter(Boolean).join(' ');

export function Progress({ value = 0, max = 100, label, tone = 'ink' }) {
  const pct = Math.max(0, Math.min(100, (value / max) * 100));
  return (
    <div className="kv-progress-wrap">
      {label && <div className="kv-progress-label"><span>{label}</span><span>{Math.round(pct)}%</span></div>}
      <div className={cx('kv-progress', 'kv-progress--' + tone)} role="progressbar" aria-valuenow={value} aria-valuemin={0} aria-valuemax={max} aria-label={label}>
        <span style={{ width: pct + '%' }} />
      </div>
    </div>
  );
}
