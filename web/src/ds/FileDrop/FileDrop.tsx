import { useRef, useState } from 'react';
import { cx } from '../cx';
import { Icon } from '../Icon/Icon';

export interface FileDropProps {
  label?: string;
  hint?: string;
  accept?: string;
  multiple?: boolean;
  onFiles?(files: File[]): void;
}

/**
 * Where people add files: project files at setup, the files page, skills.
 *
 * - `label` names what goes here; `hint` states types and limits. `accept` and `multiple` pass to the file input; `onFiles(files)` receives the list from a drop or the picker.
 * - The whole area is one target: click, Enter or drop. It tints cobalt-soft while a file hovers over it.
 * - Show what was added as `ListRow`s beneath it, each with a remove action.
 */
export function FileDrop({ label = 'Drop files here', hint, accept, multiple = true, onFiles }: FileDropProps) {
  const [over, setOver] = useState(false);
  const inp = useRef<HTMLInputElement>(null);
  return (
    <div
      className={cx('kv-drop', over && 'is-over')}
      role="button"
      tabIndex={0}
      onClick={() => inp.current?.click()}
      onKeyDown={(e) => {
        if (e.key === 'Enter' || e.key === ' ') {
          e.preventDefault();
          inp.current?.click();
        }
      }}
      onDragOver={(e) => {
        e.preventDefault();
        setOver(true);
      }}
      onDragLeave={() => setOver(false)}
      onDrop={(e) => {
        e.preventDefault();
        setOver(false);
        onFiles?.(Array.from(e.dataTransfer.files));
      }}
    >
      <Icon name="upload" size={24} />
      <span className="kv-drop-label">
        {label}{' '}
        <span className="kv-drop-or">
          or <u>choose files</u>
        </span>
      </span>
      {hint && <span className="kv-drop-hint">{hint}</span>}
      <input ref={inp} type="file" hidden accept={accept} multiple={multiple} onChange={(e) => onFiles?.(Array.from(e.target.files ?? []))} />
    </div>
  );
}
