import { useEffect, useRef, useState } from 'react';
import { Icon } from '../Icon/Icon';

export interface CodeBlockProps {
  code: string;
  language?: string;
  title?: string;
}

/**
 * A block of code, a command or a raw payload, with a copy button.
 *
 * - `code` is the text; `title` or `language` labels the header ("Terminal", "role.md", "json").
 * - Set in IBM Plex Mono on paper-raised with a hairline border; long lines scroll sideways rather than wrap.
 * - Use it for things people copy or inspect. Tool inputs and outputs in chat use the same look inside `ToolCall`.
 */
export function CodeBlock({ code = '', language, title }: CodeBlockProps) {
  const [copied, setCopied] = useState(false);
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  useEffect(() => () => clearTimeout(timer.current), []);
  const copy = () => {
    void navigator.clipboard?.writeText(code);
    setCopied(true);
    clearTimeout(timer.current);
    timer.current = setTimeout(() => setCopied(false), 1500);
  };
  return (
    <div className="kv-code">
      <div className="kv-code-head">
        <span>{title || language || 'code'}</span>
        <button type="button" className="kv-code-copy" onClick={copy}>
          <Icon name={copied ? 'check' : 'copy'} size={14} />
          {copied ? 'Copied' : 'Copy'}
        </button>
      </div>
      <pre>
        <code>{code}</code>
      </pre>
    </div>
  );
}
