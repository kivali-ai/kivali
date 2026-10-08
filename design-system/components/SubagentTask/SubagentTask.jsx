import React from 'react';
import { Icon } from '../Icon/Icon.jsx';
import { AgentState } from '../AgentState/AgentState.jsx';
const cx = (...c) => c.filter(Boolean).join(' ');

export function SubagentTask({ title, state = 'running', model, effort, elapsed, latest, result, error, sessionHref, children, defaultOpen = false }) {
  const [more, setMore] = React.useState(false);
  const [overflows, setOverflows] = React.useState(false);
  const resRef = React.useRef(null);
  React.useLayoutEffect(() => { const el = resRef.current; if (el && !more) setOverflows(el.scrollHeight > el.clientHeight + 1); }, [result, more]);
  return (
    <details className={cx('kv-sub', 'kv-sub--' + state)} open={!!defaultOpen}>
      <summary>
        <Icon name="users" size={14} />
        <span className="kv-sub-title">{title}</span>
        <span className="kv-tool-right">
          {(model || effort) && <span className="kv-sub-model">{[model, effort].filter(Boolean).join(' · ')}</span>}
          {elapsed && <span className="kv-tool-dur">{elapsed}</span>}
          <AgentState state={state} compact={state === 'done'} label={state === 'errored' ? 'Failed' : undefined} />
          <Icon name="chevron-down" size={14} />
        </span>
      </summary>
      <div className="kv-sub-body">
        {state === 'running' && latest && (
          <div className="kv-sub-latest"><span className="kv-tool-label">Latest</span><span className="kv-sub-latest-text">{latest}</span></div>
        )}
        {children && <div className="kv-sub-steps">{children}</div>}
        {error && <div className="kv-tool-part is-error"><span className="kv-tool-label">Error</span><pre>{error}</pre></div>}
        {result && (
          <div className="kv-sub-result">
            <span className="kv-tool-label">Result</span>
            <div ref={resRef} className={cx('kv-sub-result-text', !more && 'is-clamped')}>{result}</div>
            {(overflows || more) && <button className="kv-sub-more" onClick={(e) => { e.preventDefault(); setMore(!more); }}>{more ? 'Show less' : 'Show all'}</button>}
          </div>
        )}
        {sessionHref && <a className="kv-sub-session" href={sessionHref}>Open full session<Icon name="arrow-up-right" size={14} /></a>}
      </div>
    </details>
  );
}
