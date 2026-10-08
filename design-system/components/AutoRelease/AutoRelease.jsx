import React from 'react';
import { Select } from '../Select/Select.jsx';
const cx = (...c) => c.filter(Boolean).join(' ');
export const AUTO_RELEASE_STOPS = ['Now', '30s', '2m', '5m', '20m', 'Off'];

// The pacing dial: six fixed stops. Slider on desktop (one click), Select on phone.
export function AutoRelease({ value = '30s', onChange, stops = AUTO_RELEASE_STOPS, variant = 'slider', label = 'Auto-release', className }) {
  const i = Math.max(0, stops.indexOf(value));
  if (variant === 'select') {
    return (
      <div className={cx('kv-autorel', 'kv-autorel--select', className)}>
        <span className="kv-autorel-label">{label}</span>
        <Select aria-label={label} value={value} onChange={(e) => onChange && onChange(e.target.value)} options={stops.map((s) => ({ value: s, label: s }))} />
      </div>
    );
  }
  const frac = stops.length > 1 ? i / (stops.length - 1) : 0;
  return (
    <div className={cx('kv-autorel', className)} role="radiogroup" aria-label={label}>
      <span className="kv-autorel-label">{label}</span>
      <div className="kv-autorel-control">
        <div className="kv-autorel-track" style={{ '--kv-autorel-frac': frac }}>
          <span className="kv-autorel-rail" /><span className="kv-autorel-fill" />
          {stops.map((s, k) => (
            <button key={s} type="button" role="radio" aria-checked={k === i} aria-label={s} title={s}
              className="kv-autorel-stop" style={{ '--kv-autorel-at': k / (stops.length - 1) }} onClick={() => onChange && onChange(s)} />
          ))}
          <span className="kv-autorel-thumb" aria-hidden="true" />
        </div>
        <div className="kv-autorel-ticks">{stops.map((s, k) => <span key={s} className={k === i ? 'is-current' : undefined}>{s}</span>)}</div>
      </div>
    </div>
  );
}
