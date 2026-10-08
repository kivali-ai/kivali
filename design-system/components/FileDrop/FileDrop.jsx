import React from 'react';
import { Icon } from '../Icon/Icon.jsx';
const cx = (...c) => c.filter(Boolean).join(' ');

export function FileDrop({ label = 'Drop files here', hint, accept, multiple = true, onFiles }) {
  const [over, setOver] = React.useState(false);
  const inp = React.useRef(null);
  return (
    <div className={cx('kv-drop', over && 'is-over')} role="button" tabIndex={0}
      onClick={() => inp.current && inp.current.click()} onKeyDown={(e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); inp.current && inp.current.click(); } }}
      onDragOver={(e) => { e.preventDefault(); setOver(true); }} onDragLeave={() => setOver(false)}
      onDrop={(e) => { e.preventDefault(); setOver(false); onFiles && onFiles(Array.from(e.dataTransfer.files)); }}>
      <Icon name="upload" size={24} />
      <span className="kv-drop-label">{label} <span className="kv-drop-or">or <u>choose files</u></span></span>
      {hint && <span className="kv-drop-hint">{hint}</span>}
      <input ref={inp} type="file" hidden accept={accept} multiple={multiple} onChange={(e) => onFiles && onFiles(Array.from(e.target.files || []))} />
    </div>
  );
}
