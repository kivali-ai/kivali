import type { ReactNode } from 'react';
import { cx } from '../cx';
import { Icon } from '../Icon/Icon';

export type ToastTone = 'info' | 'success' | 'danger';

export interface ToastProps {
  tone?: ToastTone;
  title: ReactNode;
  action?: ReactNode;
  onDismiss?(): void;
  children?: ReactNode;
}

const TOAST_ICON: Record<ToastTone, string> = { info: 'info', success: 'circle-check', danger: 'triangle-alert' };

/**
 * A brief confirmation or error after something the person did, stacked in the bottom-right corner (bottom-center on small screens).
 *
 * - `tone`: `success` (done), `info` (neutral news, often with Undo), `danger` (the action failed). Each has its own icon.
 * - `title` is the one-line result ("Skill installed"); children add one sentence. `action` holds one `sm` button such as Undo; `onDismiss` adds a close button.
 * - Success and info toasts leave on their own after about five seconds (longer when they hold an action); danger toasts stay until dismissed. Things that need a decision belong in the inbox, not a toast.
 * - Presentational: stack toasts in one fixed region at `z-toast`.
 */
export function Toast({ tone = 'info', title, children, action, onDismiss }: ToastProps) {
  return (
    <div className={cx('kv-toast', 'kv-toast--' + tone)} role={tone === 'danger' ? 'alert' : 'status'}>
      <Icon name={TOAST_ICON[tone]} size={18} />
      <div className="kv-toast-text">
        <strong>{title}</strong>
        {children && <span>{children}</span>}
      </div>
      {action}
      {onDismiss && (
        <button type="button" className="kv-toast-close" onClick={onDismiss} aria-label="Dismiss">
          <Icon name="x" />
        </button>
      )}
    </div>
  );
}
