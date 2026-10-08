import type { CSSProperties } from 'react';
import { cx } from '../cx';
import { Select } from '../Select/Select';

export type AutoReleaseStop = 'Now' | '30s' | '2m' | '5m' | '20m' | 'Off' | (string & {});

export const AUTO_RELEASE_STOPS: string[] = ['Now', '30s', '2m', '5m', '20m', 'Off'];

export interface AutoReleaseProps {
  value?: AutoReleaseStop;
  onChange?(value: AutoReleaseStop): void;
  stops?: AutoReleaseStop[];
  variant?: 'slider' | 'select';
  label?: string;
  className?: string;
}

/**
 * The pacing dial for the queue: how long an agent-to-agent message waits before it is delivered.
 *
 * - Six fixed stops: Now, 30s, 2m, 5m, 20m, Off. Off holds everything until the person releases it; 30s is the normal state.
 * - `variant="slider"` (default) is one click per stop, for the desktop sidebar foot and the Queue header. `variant="select"` is for phone.
 * - The label sits left of the track so it reads as one control. The current stop is named in ink under the track; the others are ink-faint.
 * - Every instance writes the same server setting; queue countdowns follow it and Off cancels them.
 * - The track takes a CSS variable, `--kv-autorel-frac`, set here from the current stop.
 */
export function AutoRelease({ value = '30s', onChange, stops = AUTO_RELEASE_STOPS, variant = 'slider', label = 'Auto-release', className }: AutoReleaseProps) {
  const i = Math.max(0, stops.indexOf(value));
  if (variant === 'select') {
    return (
      <div className={cx('kv-autorel', 'kv-autorel--select', className)}>
        <span className="kv-autorel-label">{label}</span>
        <Select
          aria-label={label}
          value={value}
          onChange={(e) => onChange?.(e.target.value)}
          options={stops.map((s) => ({ value: s, label: s }))}
        />
      </div>
    );
  }
  const frac = stops.length > 1 ? i / (stops.length - 1) : 0;
  return (
    <div className={cx('kv-autorel', className)} role="radiogroup" aria-label={label}>
      <span className="kv-autorel-label">{label}</span>
      <div className="kv-autorel-control">
        <div className="kv-autorel-track" style={{ '--kv-autorel-frac': frac } as CSSProperties}>
          <span className="kv-autorel-rail" />
          <span className="kv-autorel-fill" />
          {stops.map((s, k) => (
            <button
              key={s}
              type="button"
              role="radio"
              aria-checked={k === i}
              aria-label={s}
              title={s}
              className="kv-autorel-stop"
              style={{ '--kv-autorel-at': k / (stops.length - 1) } as CSSProperties}
              onClick={() => onChange?.(s)}
            />
          ))}
          <span className="kv-autorel-thumb" aria-hidden="true" />
        </div>
        <div className="kv-autorel-ticks">
          {stops.map((s, k) => (
            <span key={s} className={k === i ? 'is-current' : undefined}>
              {s}
            </span>
          ))}
        </div>
      </div>
    </div>
  );
}
