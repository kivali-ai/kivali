import { useLayoutEffect, useRef, useState } from 'react';
import type { ReactNode } from 'react';
import { cx } from '../cx';
import { Icon } from '../Icon/Icon';

export interface ComposerProps {
  placeholder?: string;
  value?: string;
  defaultValue?: string;
  onChange?(v: string): void;
  onSend?(v: string): void;
  busy?: boolean;
  onAttach?(): void;
  footer?: ReactNode;
  disabled?: boolean;
  /** The message may go out with no text: the consumer has something else to send (staged attachments). */
  allowEmpty?: boolean;
}

/**
 * Where the person writes to an agent.
 *
 * - The text area grows with the message up to about ten lines, then scrolls. Enter sends; Shift+Enter adds a line.
 * - `onSend(text)` is called on send; `busy` swaps the send icon for three dots and blocks sending while the agent works. `onAttach` adds a paperclip; `footer` holds quiet details like the model and attachments as outline `Badge`s.
 * - Blank text never sends unless `allowEmpty` is set (the consumer has attachments staged); `onSend` then receives the empty string.
 * - The placeholder names who you are talking to ("Message Garden advisor").
 */
export function Composer({ placeholder = 'Message', value, defaultValue, onChange, onSend, busy = false, footer, onAttach, disabled = false, allowEmpty = false }: ComposerProps) {
  const [inner, setInner] = useState(defaultValue ?? '');
  const v = value ?? inner;
  const ref = useRef<HTMLTextAreaElement>(null);
  useLayoutEffect(() => {
    const t = ref.current;
    if (t) {
      t.style.height = 'auto';
      t.style.height = Math.min(t.scrollHeight, 240) + 'px';
    }
  }, [v]);
  const canSend = (allowEmpty || v.trim() !== '') && !busy && !disabled;
  const send = () => {
    if (!canSend) return;
    onSend?.(v);
    if (value == null) setInner('');
  };
  return (
    <div className={cx('kv-composer', disabled && 'is-disabled')}>
      <textarea
        ref={ref}
        rows={1}
        placeholder={placeholder}
        aria-label={placeholder}
        value={v}
        disabled={disabled}
        onChange={(e) => {
          setInner(e.target.value);
          onChange?.(e.target.value);
        }}
        onKeyDown={(e) => {
          if (e.key === 'Enter' && !e.shiftKey && !e.nativeEvent.isComposing) {
            e.preventDefault();
            send();
          }
        }}
      />
      <div className="kv-composer-bar">
        {onAttach && (
          <button type="button" className="kv-composer-icon" onClick={onAttach} aria-label="Attach files">
            <Icon name="paperclip" size={18} />
          </button>
        )}
        <div className="kv-composer-footer">{footer}</div>
        <button type="button" className="kv-composer-send" onClick={send} disabled={!canSend} aria-label={busy ? 'Working' : 'Send'}>
          {busy ? (
            <span className="kv-btn-dots" aria-hidden="true">
              <i />
              <i />
              <i />
            </span>
          ) : (
            <Icon name="send" size={16} />
          )}
        </button>
      </div>
    </div>
  );
}
