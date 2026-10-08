import React from 'react';
import { Icon } from '../Icon/Icon.jsx';
const cx = (...c) => c.filter(Boolean).join(' ');

const TOAST_ICON = { info: 'info', success: 'circle-check', danger: 'triangle-alert' };
// Presentational; stack toasts in a fixed region (z-toast).
export function Toast({ tone = 'info', title, children, action, onDismiss }) {
  return (
    <div className={cx('kv-toast', 'kv-toast--' + tone)} role={tone === 'danger' ? 'alert' : 'status'}>
      <Icon name={TOAST_ICON[tone]} size={18} />
      <div className="kv-toast-text"><strong>{title}</strong>{children && <span>{children}</span>}</div>
      {action}
      {onDismiss && <button className="kv-toast-close" onClick={onDismiss} aria-label="Dismiss"><Icon name="x" /></button>}
    </div>
  );
}
