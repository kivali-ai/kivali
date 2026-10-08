import React from 'react';
import { Icon } from '../Icon/Icon.jsx';
const cx = (...c) => c.filter(Boolean).join(' ');

const DialogCtx = React.createContext(() => {});
// Radix-style API; rendered in place with fixed positioning (no portal dependency).
export function Dialog({ open, defaultOpen = false, onOpenChange, trigger, title, description, footer, tone, children }) {
  const [inner, setInner] = React.useState(defaultOpen);
  const isOpen = open !== undefined ? open : inner;
  const set = (v) => { if (open === undefined) setInner(v); onOpenChange && onOpenChange(v); };
  const ref = React.useRef(null);
  React.useEffect(() => {
    if (!isOpen) return;
    ref.current && ref.current.focus();
    const k = (e) => { if (e.key === 'Escape') set(false); };
    document.addEventListener('keydown', k);
    return () => document.removeEventListener('keydown', k);
  }, [isOpen]);
  return (
    <DialogCtx.Provider value={() => set(false)}>
      {trigger && React.cloneElement(trigger, { onClick: (e) => { trigger.props.onClick && trigger.props.onClick(e); set(true); } })}
      {isOpen && <>
        <div className="kv-dialog-overlay" onClick={() => set(false)} />
        <div ref={ref} role="dialog" aria-modal="true" tabIndex={-1} className={cx('kv-dialog', tone && 'kv-dialog--' + tone)}>
          <div className="kv-dialog-head">
            <h2 className="kv-dialog-title">{title}</h2>
            <button className="kv-dialog-close" aria-label="Close" onClick={() => set(false)}><Icon name="x" /></button>
          </div>
          {description && <p className="kv-dialog-desc">{description}</p>}
          {children && <div className="kv-dialog-body">{children}</div>}
          {footer && <div className="kv-dialog-foot">{footer}</div>}
        </div>
      </>}
    </DialogCtx.Provider>
  );
}
// Wraps a button so clicking it closes the enclosing Dialog. <DialogClose asChild><Button>Cancel</Button></DialogClose>
export function DialogClose({ children }) {
  const close = React.useContext(DialogCtx);
  const child = React.Children.only(children);
  return React.cloneElement(child, { onClick: (e) => { child.props.onClick && child.props.onClick(e); close(); } });
}
