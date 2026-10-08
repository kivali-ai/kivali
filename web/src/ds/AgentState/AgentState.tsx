import { cx } from '../cx';
import { Icon } from '../Icon/Icon';

export type AgentRunState = 'running' | 'idle' | 'queued' | 'blocked' | 'held' | 'errored' | 'done' | 'cancelled';

export interface AgentStateProps {
  state: AgentRunState;
  label?: string;
  compact?: boolean;
  className?: string;
}

const STATE_LABEL: Record<AgentRunState, string> = {
  running: 'Working',
  idle: 'Idle',
  queued: 'Queued',
  blocked: 'Blocked',
  held: 'On hold',
  errored: 'Needs help',
  done: 'Done',
  cancelled: 'Cancelled',
};

function Glyph({ state }: { state: AgentRunState }) {
  switch (state) {
    case 'errored':
      return <Icon name="triangle-alert" size={14} />;
    case 'done':
      return <Icon name="check" size={14} />;
    case 'cancelled':
      return <Icon name="x" size={14} />;
    case 'blocked':
      return (
        <>
          <i />
          <i />
          <b className="kv-state-wall" />
        </>
      );
    case 'held':
      return (
        <>
          <b className="kv-state-bar" />
          <b className="kv-state-bar" />
        </>
      );
    case 'idle':
      return <i />;
    default:
      return (
        <>
          <i />
          <i />
          <i />
        </>
      );
  }
}

/**
 * An agent's run state as three small dots plus a word: the logo's honey dots at work. Never a bare colored light.
 *
 * - `state`: `running` (dots bounce in signal, "Working"), `idle` (a single resting dot), `queued` (three hollow cobalt rings waiting in line), `blocked` (two dots stopped at a wall, signal-ink), `held` (a pause mark), `errored` ("Needs help" in danger with an icon), `done` (success check), `cancelled`.
 * - `label` overrides the word ("Writing report"); keep it short.
 * - `compact` shows the dots only, with the word as the accessible name and tooltip. Use it in dense lists and the org tree; use the full form everywhere else.
 * - Only `running` moves. Every state that is not normal work has a shape of its own, so no still state can be mistaken for a frame of the running animation.
 * - If the product's state list changes, map each new state to the closest meaning and add a word; add a new glyph only for a genuinely new meaning.
 */
export function AgentState({ state = 'idle', label, compact = false, className }: AgentStateProps) {
  const text = label || STATE_LABEL[state] || state;
  return (
    <span
      className={cx('kv-state', 'kv-state--' + state, compact && 'kv-state--compact', className)}
      role="status"
      aria-label={compact ? text : undefined}
      title={compact ? text : undefined}
    >
      <span className="kv-state-glyph" aria-hidden="true">
        <Glyph state={state} />
      </span>
      {!compact && <span className="kv-state-text">{text}</span>}
    </span>
  );
}
