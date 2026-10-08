import React from 'react';

const POS = {
  top: { bottom: 'calc(100% + 6px)', left: '50%', transform: 'translateX(-50%)' },
  bottom: { top: 'calc(100% + 6px)', left: '50%', transform: 'translateX(-50%)' },
  left: { right: 'calc(100% + 6px)', top: '50%', transform: 'translateY(-50%)' },
  right: { left: 'calc(100% + 6px)', top: '50%', transform: 'translateY(-50%)' },
};
export function Tooltip({ content, children, side = 'top', open }) {
  const [hover, setHover] = React.useState(false);
  const t = React.useRef();
  const show = open !== undefined ? open : hover;
  return (
    <span style={{ position: 'relative', display: 'inline-flex' }}
      onMouseEnter={() => { t.current = setTimeout(() => setHover(true), 300); }}
      onMouseLeave={() => { clearTimeout(t.current); setHover(false); }}
      onFocus={() => setHover(true)} onBlur={() => setHover(false)}>
      {children}
      {show && <span role="tooltip" className="kv-tooltip" style={{ position: 'absolute', whiteSpace: 'nowrap', ...POS[side] }}>{content}</span>}
    </span>
  );
}
