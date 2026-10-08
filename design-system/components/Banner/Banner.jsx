import React from 'react';
import { Icon } from '../Icon/Icon.jsx';
const cx = (...c) => c.filter(Boolean).join(' ');

const BANNER_ICON = { info: 'info', warning: 'triangle-alert', danger: 'triangle-alert', success: 'circle-check' };
export function Banner({ tone = 'info', title, action, onDismiss, className, children }) {
  return (
    <div className={cx('kv-banner', 'kv-banner--' + tone, className)} role={tone === 'danger' || tone === 'warning' ? 'alert' : 'status'}>
      <Icon name={BANNER_ICON[tone]} size={18} />
      <div className="kv-banner-text">{title && <strong>{title}</strong>}{children && <span>{children}</span>}</div>
      {action && <div className="kv-banner-action">{action}</div>}
      {onDismiss && <button className="kv-banner-close" onClick={onDismiss} aria-label="Dismiss"><Icon name="x" /></button>}
    </div>
  );
}
