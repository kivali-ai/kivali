import React from 'react';
import { Icon } from '../Icon/Icon.jsx';
import { AgentState } from '../AgentState/AgentState.jsx';
const cx = (...c) => c.filter(Boolean).join(' ');

const TOOL_STATE = { running: 'running', done: 'done', error: 'errored' };
export function ToolCall({ name, status = 'done', summary, input, output, error, duration, defaultOpen = false }) {
  const fmt = (v) => (typeof v === 'string' ? v : JSON.stringify(v, null, 2));
  return (
    <details className={cx('kv-tool', 'kv-tool--' + status)} open={defaultOpen}>
      <summary>
        <Icon name="wrench" size={14} />
        <span className="kv-tool-name">{name}</span>
        {summary && <span className="kv-tool-summary">{summary}</span>}
        <span className="kv-tool-right">
          {duration && status !== 'running' && <span className="kv-tool-dur">{duration}</span>}
          <AgentState state={TOOL_STATE[status]} compact label={status === 'error' ? 'Failed' : status === 'running' ? 'Running' : 'Done'} />
          <Icon name="chevron-down" size={14} />
        </span>
      </summary>
      <div className="kv-tool-body">
        {input != null
          ? <div className="kv-tool-part"><span className="kv-tool-label">Input</span><pre>{fmt(input)}</pre></div>
          : <div className="kv-tool-part"><span className="kv-tool-label">Input</span><span className="kv-tool-none">No input</span></div>}
        {error ? <div className="kv-tool-part is-error"><span className="kv-tool-label">Error</span><pre>{error}</pre></div>
          : output != null ? <div className="kv-tool-part"><span className="kv-tool-label">Output</span><pre>{fmt(output)}</pre></div>
            : status === 'running' ? <div className="kv-tool-part"><span className="kv-tool-label">Output</span><span className="kv-tool-none"><AgentState state="running" compact label="Waiting for output" /> Waiting for output</span></div>
              : <div className="kv-tool-part"><span className="kv-tool-label">Output</span><span className="kv-tool-none">No output</span></div>}
      </div>
    </details>
  );
}
