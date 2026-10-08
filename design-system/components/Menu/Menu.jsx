import React from 'react';
import { Icon } from '../Icon/Icon.jsx';
const cx = (...c) => c.filter(Boolean).join(' ');

// Shared dropdown behaviour (outside click + Escape close). Rendered in place, absolutely positioned.
export function usePopover(defaultOpen = false) {
  const [open, setOpen] = React.useState(!!defaultOpen);
  const ref = React.useRef(null);
  React.useEffect(() => {
    if (!open) return;
    const down = (e) => { if (ref.current && !ref.current.contains(e.target)) setOpen(false); };
    const key = (e) => { if (e.key === 'Escape') setOpen(false); };
    document.addEventListener('mousedown', down); document.addEventListener('keydown', key);
    return () => { document.removeEventListener('mousedown', down); document.removeEventListener('keydown', key); };
  }, [open]);
  return { open, setOpen, ref };
}
export function MenuItem({ icon, label, shortcut, right, danger, disabled, onSelect, close, children }) {
  const [hi, setHi] = React.useState(false);
  return (
    <div role="menuitem" tabIndex={-1} className={cx('kv-menu-item', danger && 'is-danger')} data-highlighted={hi && !disabled ? '' : undefined}
      data-disabled={disabled ? '' : undefined} onMouseEnter={() => setHi(true)} onMouseLeave={() => setHi(false)}
      onClick={() => { if (disabled) return; onSelect && onSelect(); close && close(); }}>
      {children || <>{icon && <Icon name={icon} />}<span>{label}</span>
        {shortcut && <span className="kv-menu-right kv-menu-kbd">{shortcut}</span>}{right}</>}
    </div>
  );
}
export function Menu({ trigger, items = [], align = 'end', side = 'bottom', defaultOpen }) {
  const p = usePopover(defaultOpen);
  const close = () => p.setOpen(false);
  return (
    <span ref={p.ref} style={{ position: 'relative', display: 'inline-block' }}>
      {React.cloneElement(trigger, { onClick: () => p.setOpen(!p.open), 'aria-expanded': p.open, 'aria-haspopup': 'menu' })}
      {p.open && (
        <div role="menu" className="kv-menu" style={{ position: 'absolute', zIndex: 'var(--z-menu)', ...(side === 'top' ? { bottom: 'calc(100% + 6px)' } : side === 'left' ? { right: 'calc(100% + 6px)', top: 0 } : side === 'right' ? { left: 'calc(100% + 6px)', top: 0 } : { top: 'calc(100% + 6px)' }), ...((side === 'top' || side === 'bottom') ? (align === 'start' ? { left: 0 } : align === 'center' ? { left: '50%', transform: 'translateX(-50%)' } : { right: 0 }) : {}) }}>
          {items.map((it, i) => it.separator ? <div key={i} className="kv-menu-sep" role="separator" /> : <MenuItem key={i} {...it} close={close} />)}
        </div>
      )}
    </span>
  );
}
