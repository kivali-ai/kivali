import { useLayoutEffect, useRef, useState } from 'react';
import type { ReactNode } from 'react';
import { cx } from '../cx';
import { AgentState } from '../AgentState/AgentState';
import type { AgentRunState } from '../AgentState/AgentState';
import { Icon } from '../Icon/Icon';

export interface SubagentTaskProps {
  title: string;
  state?: AgentRunState;
  model?: string;
  effort?: string;
  elapsed?: string;
  latest?: string;
  result?: ReactNode;
  error?: string;
  sessionHref?: string;
  defaultOpen?: boolean;
  children?: ReactNode;
}

/**
 * Work an agent handed to a short-lived subagent: one block per task, inside the agent's `Message`.
 *
 * - Header, always visible: what was delegated (`title`), the `model` and `effort` it runs with as a quiet mono pill ("sonnet · medium"), the `elapsed` time, and its state with the `AgentState` vocabulary (`running`, `done`, `errored`).
 * - It starts collapsed in every state. Opened while running, it shows `latest`: one line naming the subagent's most recent step, followed by the steps so far as children (`ToolCall`, `Thinking`). The full step-by-step lives in the session, not the chat.
 * - Opened when done, it shows the `result`: the subagent's final answer, clamped to four lines with Show all. The result is what the parent agent received.
 * - When it fails, the header says Failed in danger; opened, it shows the `error`.
 * - `sessionHref` adds "Open full session", the complete transcript. Several tasks started together sit in a `SubagentGroup`.
 */
export function SubagentTask({ title, state = 'running', model, effort, elapsed, latest, result, error, sessionHref, children, defaultOpen = false }: SubagentTaskProps) {
  const [more, setMore] = useState(false);
  const [overflows, setOverflows] = useState(false);
  const resRef = useRef<HTMLDivElement>(null);
  useLayoutEffect(() => {
    const el = resRef.current;
    if (el && !more) setOverflows(el.scrollHeight > el.clientHeight + 1);
  }, [result, more]);
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
          <div className="kv-sub-latest">
            <span className="kv-tool-label">Latest</span>
            <span className="kv-sub-latest-text">{latest}</span>
          </div>
        )}
        {children && <div className="kv-sub-steps">{children}</div>}
        {error && (
          <div className="kv-tool-part is-error">
            <span className="kv-tool-label">Error</span>
            <pre>{error}</pre>
          </div>
        )}
        {result && (
          <div className="kv-sub-result">
            <span className="kv-tool-label">Result</span>
            <div ref={resRef} className={cx('kv-sub-result-text', !more && 'is-clamped')}>
              {result}
            </div>
            {(overflows || more) && (
              <button
                type="button"
                className="kv-sub-more"
                onClick={(e) => {
                  e.preventDefault();
                  setMore(!more);
                }}
              >
                {more ? 'Show less' : 'Show all'}
              </button>
            )}
          </div>
        )}
        {sessionHref && (
          <a className="kv-sub-session" href={sessionHref}>
            Open full session
            <Icon name="arrow-up-right" size={14} />
          </a>
        )}
      </div>
    </details>
  );
}
