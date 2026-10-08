import React from 'react';
import { Icon } from '../Icon/Icon.jsx';
const cx = (...c) => c.filter(Boolean).join(' ');

export function Composer({ placeholder = 'Message', value, defaultValue, onChange, onSend, busy = false, footer, onAttach, disabled = false, allowEmpty = false }) {
  const [inner, setInner] = React.useState(defaultValue || '');
  const v = value != null ? value : inner;
  const ref = React.useRef(null);
  React.useLayoutEffect(() => { const t = ref.current; if (t) { t.style.height = 'auto'; t.style.height = Math.min(t.scrollHeight, 240) + 'px'; } }, [v]);
  const canSend = (allowEmpty || v.trim() !== '') && !busy && !disabled;
  const send = () => { if (!canSend) return; onSend && onSend(v); if (value == null) setInner(''); };
  return (
    <div className={cx('kv-composer', disabled && 'is-disabled')}>
      <textarea ref={ref} rows={1} placeholder={placeholder} aria-label={placeholder} value={v} disabled={disabled}
        onChange={(e) => { setInner(e.target.value); onChange && onChange(e.target.value); }}
        onKeyDown={(e) => { if (e.key === 'Enter' && !e.shiftKey && !e.nativeEvent.isComposing) { e.preventDefault(); send(); } }} />
      <div className="kv-composer-bar">
        {onAttach && <button type="button" className="kv-composer-icon" onClick={onAttach} aria-label="Attach files"><Icon name="paperclip" size={18} /></button>}
        <div className="kv-composer-footer">{footer}</div>
        <button type="button" className="kv-composer-send" onClick={send} disabled={!canSend} aria-label={busy ? 'Working' : 'Send'}>
          {busy ? <span className="kv-btn-dots" aria-hidden="true"><i /><i /><i /></span> : <Icon name="send" size={16} />}
        </button>
      </div>
    </div>
  );
}
