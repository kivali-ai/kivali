import type { ReactNode } from 'react';
import { cx } from '../cx';
import { Icon } from '../Icon/Icon';

export type BannerTone = 'info' | 'warning' | 'danger' | 'success';

export interface BannerProps {
  tone?: BannerTone;
  title?: ReactNode;
  action?: ReactNode;
  onDismiss?(): void;
  children?: ReactNode;
  className?: string;
}

const BANNER_ICON: Record<BannerTone, string> = {
  info: 'info',
  warning: 'triangle-alert',
  danger: 'triangle-alert',
  success: 'circle-check',
};

/**
 * A full-width message about the page or the whole org: a notice, a warning, an error or a confirmation.
 *
 * - `tone`: `info` (cobalt-soft), `warning` (signal-soft, needs the person), `danger` (something failed), `success`. Each carries its own icon, so color is never the only cue.
 * - `title` is the one-line point; children add a sentence. Errors say what happened, then what happens next or what to do.
 * - `action` holds one `sm` button; `onDismiss` adds a close button.
 * - Environment and system warnings (production, clock drift) are `danger` or `warning` banners at the top of the page.
 */
export function Banner({ tone = 'info', title, action, onDismiss, className, children }: BannerProps) {
  return (
    <div className={cx('kv-banner', 'kv-banner--' + tone, className)} role={tone === 'danger' || tone === 'warning' ? 'alert' : 'status'}>
      <Icon name={BANNER_ICON[tone]} size={18} />
      <div className="kv-banner-text">
        {title && <strong>{title}</strong>}
        {children && <span>{children}</span>}
      </div>
      {action && <div className="kv-banner-action">{action}</div>}
      {onDismiss && (
        <button type="button" className="kv-banner-close" onClick={onDismiss} aria-label="Dismiss">
          <Icon name="x" />
        </button>
      )}
    </div>
  );
}
