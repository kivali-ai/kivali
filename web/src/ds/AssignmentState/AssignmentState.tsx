import { cx } from '../cx';

export type AssignmentReadiness = 'ready' | 'blocked' | 'held' | 'done' | 'dropped';

const STATE_LABEL: Record<AssignmentReadiness, string> = {
  ready: 'Ready',
  blocked: 'Blocked',
  held: 'On hold',
  done: 'Done',
  dropped: 'Dropped',
};

/** The circle glyph for an assignment state. Circles are work (agents are dots). */
export function AssignmentGlyph({ state, size = 14 }: { state: AssignmentReadiness; size?: number }) {
  const c = 7;
  const r = 5.5;
  return (
    <svg className="kv-assignment-glyph" width={size} height={size} viewBox="0 0 14 14" aria-hidden="true">
      {state === 'done' ? (
        <>
          <circle cx={c} cy={c} r={6.5} className="g-fill" />
          <path d="M4.3 7.2l1.9 1.9 3.6-3.8" className="g-on" />
        </>
      ) : (
        <circle cx={c} cy={c} r={r} className="g-ring" />
      )}
      {state === 'ready' && <circle cx={c} cy={c} r={2} className="g-dot" />}
      {state === 'blocked' && <line x1={c} y1={3.6} x2={c} y2={10.4} className="g-line" />}
      {state === 'held' && (
        <>
          <line x1={5.6} y1={4.8} x2={5.6} y2={9.2} className="g-line" />
          <line x1={8.4} y1={4.8} x2={8.4} y2={9.2} className="g-line" />
        </>
      )}
      {state === 'dropped' && <line x1={3.2} y1={10.8} x2={10.8} y2={3.2} className="g-line" />}
    </svg>
  );
}

export interface AssignmentStateProps {
  state: AssignmentReadiness;
  label?: string;
  compact?: boolean;
}

/**
 * Where an assignment stands, as a small circle plus a word. Assignments are circles; agents are dots (`AgentState`), so the two never read as each other.
 *
 * - `state`: `ready` (a filled center in cobalt: can move now), `blocked` (a wall through the circle in signal-ink: waiting on children or other assignments), `held` (a pause mark in signal-ink), `done` (a filled success check), `dropped` (a slash in ink-muted).
 * - These are derived from the tracker, never set by hand: blocked, held and ready follow from the tree. `label` overrides the word; `compact` shows the circle only with the word as its accessible name.
 * - Like agent states, the list may change; map new states to the nearest meaning before adding a glyph.
 */
export function AssignmentState({ state = 'ready', label, compact = false }: AssignmentStateProps) {
  const text = label || STATE_LABEL[state] || state;
  return (
    <span className={cx('kv-istate', 'kv-istate--' + state)} role="status" aria-label={compact ? text : undefined} title={compact ? text : undefined}>
      <AssignmentGlyph state={state} />
      {!compact && <span>{text}</span>}
    </span>
  );
}
