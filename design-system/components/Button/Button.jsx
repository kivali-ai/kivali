import React from 'react';
const cx = (...c) => c.filter(Boolean).join(' ');

export const Button = React.forwardRef(function Button(
  { variant = 'secondary', size = 'md', icon, iconOnly = false, loading = false, className, children, ...rest }, ref) {
  return (
    <button ref={ref} className={cx('kv-btn', 'kv-btn--' + variant, 'kv-btn--' + size, iconOnly && 'kv-btn--icon', loading && 'is-loading', className)}
      aria-busy={loading || undefined} {...rest}>
      {loading ? <span className="kv-btn-dots" aria-hidden="true"><i /><i /><i /></span> : icon}
      {iconOnly ? null : <span className="kv-btn-label">{children}</span>}
    </button>
  );
});
