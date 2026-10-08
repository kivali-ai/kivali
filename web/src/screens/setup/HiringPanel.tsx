import { AgentAvatar, AgentState, Banner, Button, Progress, Text } from '../../ds';
import type { AgentRunState } from '../../ds';
import type { SetupProgress } from '../../api/types.gen';
import { CHIEF_OF_STAFF } from '../../lib/agentIdentity';
import { formatElapsed } from './format';
import { Actions, StepHead } from './parts';

const STAGE_STATE: Record<string, AgentRunState> = {
  done: 'done',
  running: 'running',
  queued: 'queued',
  failed: 'errored',
};

export interface HiringPanelProps {
  /** Null until the first answer from /progress. */
  progress: SetupProgress | null;
  /** True while Try again is re-posting the hire. */
  retrying?: boolean;
  onBack(): void;
  onRetry(): void;
}

/** The seed in flight: staged lines with their states, elapsed time, and on failure what happened and who can fix it. */
export function HiringPanel({ progress, retrying = false, onBack, onRetry }: HiringPanelProps) {
  const stages = progress?.stages ?? [];
  const failed = progress?.state === 'failed';
  const settled = stages.filter((s) => s.state === 'done').length + (stages.some((s) => s.state === 'running') ? 0.5 : 0);
  return (
    <>
      <StepHead title={failed ? 'Your Chief of Staff could not start' : 'Almost there'} />
      <div className="app-setup-hiring">
        <div className="app-setup-hiring-head">
          <AgentAvatar {...CHIEF_OF_STAFF} size={32} />
          <Text className="app-setup-hiring-title">Hiring your Chief of Staff</Text>
          <Text variant="label" tone="muted">
            {formatElapsed(progress?.elapsed_s ?? 0)}
          </Text>
        </div>
        <Progress aria-label="Hiring your Chief of Staff" value={settled} max={Math.max(stages.length, 1)} tone="cobalt" />
        <ul className="app-setup-stages">
          {stages.map((s) => (
            <li key={s.label} className={'app-setup-stage' + (s.state === 'queued' ? ' is-queued' : '')}>
              <span className="app-setup-stage-state">
                <AgentState state={STAGE_STATE[s.state] ?? 'queued'} compact />
              </span>
              <span>{s.label}</span>
            </li>
          ))}
        </ul>
      </div>
      {failed ? (
        <>
          <Banner tone="danger" title={progress?.error || 'Your Chief of Staff could not start.'}>
            {progress?.who ? 'Who can fix it: ' + progress.who + '.' : undefined}
          </Banner>
          <Actions>
            <Button onClick={onBack}>Back</Button>
            <span className="app-setup-spacer" />
            <Button variant="primary" loading={retrying} disabled={retrying} onClick={onRetry}>
              Try again
            </Button>
          </Actions>
        </>
      ) : (
        <Text as="p" variant="caption" tone="muted">
          This takes 30 to 60 seconds. You can leave this page; it carries on.
        </Text>
      )}
    </>
  );
}
