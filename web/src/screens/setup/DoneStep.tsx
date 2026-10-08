import { Link } from 'react-router';
import { AgentAvatar, Button } from '../../ds';
import { CHIEF_OF_STAFF } from '../../lib/agentIdentity';
import { Actions, StepHead } from './parts';

export interface DoneStepProps {
  fileCount: number;
  /** Whether the Chief of Staff was asked to write the handbook from the files. */
  fromFiles: boolean;
  /** A personal team: its Chief of Staff asks a few questions, then drafts About you. */
  personal?: boolean;
  onHome(): void;
}

/** Done: one button into Home, and what Needs you will hold. */
export function DoneStep({ fileCount, fromFiles, personal = false, onHome }: DoneStepProps) {
  const drafting = fromFiles && fileCount > 0;
  let next = 'It will introduce itself in Home, and Needs you will show its first message.';
  if (personal) {
    next = 'It will send you a few questions in Needs you, then propose an About you section for you to approve.';
  } else if (drafting) {
    next =
      'It read your ' +
      (fileCount === 1 ? 'file' : fileCount + ' files') +
      ' and is drafting a handbook. The proposal will be the first thing in Needs you. If the files did not say enough, it asks you a few questions first.';
  }
  return (
    <>
      <div className="app-setup-lead">
        <AgentAvatar {...CHIEF_OF_STAFF} size={56} />
      </div>
      <StepHead title="Your Chief of Staff is hired">{next}</StepHead>
      <Actions>
        <Link className="app-setup-link" to="/agents/chief-of-staff">
          Open its chat
        </Link>
        <span className="app-setup-spacer" />
        <Button variant="primary" onClick={onHome}>
          Go to Home
        </Button>
      </Actions>
    </>
  );
}
