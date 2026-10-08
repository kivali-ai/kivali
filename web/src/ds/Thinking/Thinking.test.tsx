import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { Thinking } from './Thinking';

describe('Thinking', () => {
  it('says Thinking while active', () => {
    render(<Thinking active />);
    expect(screen.getAllByText('Thinking').length).toBeGreaterThan(0);
  });

  it('says how long it thought once done, and holds the reasoning', () => {
    const { container } = render(
      <Thinking active={false} seconds={12}>
        The bounce rate is under the threshold.
      </Thinking>,
    );
    expect(screen.getByText('Thought for 12s')).toBeInTheDocument();
    expect(screen.getByText('The bounce rate is under the threshold.')).toBeInTheDocument();
    expect(container.querySelector('details')).not.toHaveAttribute('open');
  });

  it('opens when asked', () => {
    const { container } = render(<Thinking active={false} defaultOpen>Reason</Thinking>);
    expect(container.querySelector('details')).toHaveAttribute('open');
  });
});
