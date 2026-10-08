import type { AriaAttributes } from 'react';
import { cx } from '../cx';

export interface ProgressProps extends Pick<AriaAttributes, 'aria-label' | 'aria-labelledby' | 'aria-describedby' | 'aria-valuetext'> {
  value: number;
  max?: number;
  label?: string;
  tone?: 'ink' | 'cobalt' | 'success';
}

/**
 * A thin bar for something measurable: an upload, budget used, setup steps.
 *
 * - `value` and `max` (default 100), a `label` shown above with the percentage, and `tone`: `ink` (default), `cobalt`, `success`.
 * - Without a visible `label`, name the bar with `aria-label`; `aria-valuetext` replaces the percentage a screen reader hears ("Step 2 of 4"). `aria-labelledby` and `aria-describedby` go on the bar too.
 * - For acceptance items on an assignment, use `AcceptanceMeter`, which shows satisfied, claimed and unclaimed separately.
 * - For work with no measurable end, use the button's loading dots or `AgentState`, not an endless bar.
 */
export function Progress({ value = 0, max = 100, label, tone = 'ink', 'aria-label': ariaLabel, ...aria }: ProgressProps) {
  const pct = Math.max(0, Math.min(100, (value / max) * 100));
  return (
    <div className="kv-progress-wrap">
      {label && (
        <div className="kv-progress-label">
          <span>{label}</span>
          <span>{Math.round(pct)}%</span>
        </div>
      )}
      <div
        className={cx('kv-progress', 'kv-progress--' + tone)}
        role="progressbar"
        aria-valuenow={value}
        aria-valuemin={0}
        aria-valuemax={max}
        aria-label={ariaLabel ?? label}
        {...aria}
      >
        <span style={{ width: pct + '%' }} />
      </div>
    </div>
  );
}
