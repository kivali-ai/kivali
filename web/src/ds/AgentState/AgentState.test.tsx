import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { AgentState } from './AgentState';
import type { AgentRunState } from './AgentState';

const words: Record<AgentRunState, string> = {
  running: 'Working',
  idle: 'Idle',
  queued: 'Queued',
  blocked: 'Blocked',
  held: 'On hold',
  errored: 'Needs help',
  done: 'Done',
  cancelled: 'Cancelled',
};

describe('AgentState', () => {
  it.each(Object.entries(words))('says the word for %s', (state, word) => {
    render(<AgentState state={state as AgentRunState} />);
    expect(screen.getByRole('status')).toHaveTextContent(word);
  });

  it('takes a custom label', () => {
    render(<AgentState state="running" label="Writing report" />);
    expect(screen.getByRole('status')).toHaveTextContent('Writing report');
  });

  it('names the state for assistive technology when compact', () => {
    render(<AgentState state="blocked" compact />);
    const el = screen.getByRole('status', { name: 'Blocked' });
    expect(el).toHaveAttribute('title', 'Blocked');
    expect(el).not.toHaveTextContent('Blocked');
  });
});
