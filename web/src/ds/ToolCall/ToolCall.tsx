import type { ReactNode } from 'react';
import { cx } from '../cx';
import { AgentState } from '../AgentState/AgentState';
import type { AgentRunState } from '../AgentState/AgentState';
import { Icon } from '../Icon/Icon';

export type ToolCallStatus = 'running' | 'done' | 'error';

export interface ToolCallProps {
  name: string;
  status?: ToolCallStatus;
  summary?: string;
  input?: unknown;
  output?: unknown;
  error?: string;
  duration?: string;
  defaultOpen?: boolean;
  /** Drawn last in the opened body: an action on the call, such as loading the rest of a cut payload. */
  more?: ReactNode;
}

const TOOL_STATE: Record<ToolCallStatus, AgentRunState> = { running: 'running', done: 'done', error: 'errored' };

const fmt = (v: unknown): string => (typeof v === 'string' ? v : (JSON.stringify(v, null, 2) ?? ''));

/**
 * One tool an agent used: collapsed to a single line, openable to see what went in and what came out.
 *
 * - `name` (mono), a short `summary` of what it acted on, `status` (`running`, `done`, `error`) shown with the `AgentState` dots, and `duration` once finished.
 * - `input` and `output` accept text or objects (shown as formatted JSON); `error` replaces the output and turns the block's edge and payload danger. Failed calls stay collapsed too; the header's Failed state and red edge flag them.
 * - Several consecutive calls stack with an 8px gap. Keep them inside the agent's `Message`, below its text.
 * - `more` goes after the output, inside the opened body.
 * - Opened while running, it shows the input it was called with and "Waiting for output" with running dots; a call with no input or output says so rather than showing an empty box.
 */
export function ToolCall({ name, status = 'done', summary, input, output, error, duration, defaultOpen = false, more }: ToolCallProps) {
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
        {input != null ? (
          <div className="kv-tool-part">
            <span className="kv-tool-label">Input</span>
            <pre>{fmt(input)}</pre>
          </div>
        ) : (
          <div className="kv-tool-part">
            <span className="kv-tool-label">Input</span>
            <span className="kv-tool-none">No input</span>
          </div>
        )}
        {error ? (
          <div className="kv-tool-part is-error">
            <span className="kv-tool-label">Error</span>
            <pre>{error}</pre>
          </div>
        ) : output != null ? (
          <div className="kv-tool-part">
            <span className="kv-tool-label">Output</span>
            <pre>{fmt(output)}</pre>
          </div>
        ) : status === 'running' ? (
          <div className="kv-tool-part">
            <span className="kv-tool-label">Output</span>
            <span className="kv-tool-none">
              <AgentState state="running" compact label="Waiting for output" /> Waiting for output
            </span>
          </div>
        ) : (
          <div className="kv-tool-part">
            <span className="kv-tool-label">Output</span>
            <span className="kv-tool-none">No output</span>
          </div>
        )}
        {more}
      </div>
    </details>
  );
}
