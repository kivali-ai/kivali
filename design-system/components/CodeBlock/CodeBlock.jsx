import React from 'react';
import { Icon } from '../Icon/Icon.jsx';

export function CodeBlock({ code = '', language, title }) {
  const [copied, setCopied] = React.useState(false);
  return (
    <div className="kv-code">
      <div className="kv-code-head"><span>{title || language || 'code'}</span>
        <button className="kv-code-copy" onClick={() => { navigator.clipboard && navigator.clipboard.writeText(code); setCopied(true); setTimeout(() => setCopied(false), 1500); }}>
          <Icon name={copied ? 'check' : 'copy'} size={14} />{copied ? 'Copied' : 'Copy'}
        </button>
      </div>
      <pre><code>{code}</code></pre>
    </div>
  );
}
