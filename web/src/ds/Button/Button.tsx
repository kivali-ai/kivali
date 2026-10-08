import { forwardRef } from 'react';
import type { ButtonHTMLAttributes, ReactNode } from 'react';
import { cx } from '../cx';

export interface ButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: 'primary' | 'secondary' | 'ghost' | 'danger';
  size?: 'sm' | 'md';
  icon?: ReactNode;
  iconOnly?: boolean;
  loading?: boolean;
}

/**
 * Buttons start or confirm an action; the label is a verb in sentence case ("Hire agent", "Release all").
 *
 * - `variant`: `primary` (an ink fill; one per view, the main action), `secondary` (default, bordered), `ghost` (soft line-colored fill, no border: toolbars and skip-type actions), `danger` (only for destructive actions such as offboarding or deleting files; pair it with a confirm `Dialog`).
 * - `size`: `md` (36px) is the default everywhere; `sm` (28px) only inside dense rows and cards. Never mix sizes within one group of buttons.
 * - Cancel in a dialog or form is `secondary`, not `ghost`, so both choices look like buttons.
 * - `loading` swaps the icon for three bouncing dots and sets `aria-busy`; keep the label.
 * - `icon` puts an icon before the label; `iconOnly` hides the label (give an `aria-label`).
 */
export const Button = forwardRef<HTMLButtonElement, ButtonProps>(function Button(
  { variant = 'secondary', size = 'md', icon, iconOnly = false, loading = false, className, children, ...rest },
  ref,
) {
  return (
    <button
      ref={ref}
      className={cx('kv-btn', 'kv-btn--' + variant, 'kv-btn--' + size, iconOnly && 'kv-btn--icon', loading && 'is-loading', className)}
      aria-busy={loading || undefined}
      {...rest}
    >
      {loading ? (
        <span className="kv-btn-dots" aria-hidden="true">
          <i />
          <i />
          <i />
        </span>
      ) : (
        icon
      )}
      {iconOnly ? null : <span className="kv-btn-label">{children}</span>}
    </button>
  );
});
