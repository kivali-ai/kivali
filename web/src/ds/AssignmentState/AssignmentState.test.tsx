import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { AssignmentGlyph, AssignmentState } from './AssignmentState';
import type { AssignmentReadiness } from './AssignmentState';

const words: Record<AssignmentReadiness, string> = {
  ready: 'Ready',
  blocked: 'Blocked',
  held: 'On hold',
  done: 'Done',
  dropped: 'Dropped',
};

describe('AssignmentState', () => {
  it.each(Object.entries(words))('says the word for %s', (state, word) => {
    render(<AssignmentState state={state as AssignmentReadiness} />);
    expect(screen.getByRole('status')).toHaveTextContent(word);
  });

  it('names the state when compact', () => {
    render(<AssignmentState state="held" compact />);
    expect(screen.getByRole('status', { name: 'On hold' })).not.toHaveTextContent('On hold');
  });

  it('exports the glyph', () => {
    const { container } = render(<AssignmentGlyph state="ready" size={12} />);
    expect(container.querySelector('svg')).toHaveAttribute('width', '12');
  });
});
