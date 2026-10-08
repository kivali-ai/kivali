import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { DayBars } from './DayBars';

describe('DayBars', () => {
  it('draws one bar per day with the exact value as a title', () => {
    const { container } = render(<DayBars values={[12, 18.5, 9]} ariaLabel="Spend per day" />);
    expect(screen.getByRole('img', { name: 'Spend per day' })).toBeInTheDocument();
    expect(container.querySelectorAll('rect')).toHaveLength(3);
    expect(screen.getByText('$18.5')).toBeInTheDocument();
  });

  it('labels the axis and the x positions', () => {
    render(<DayBars values={[1, 2, 3]} labels={[[0, 'Sep 1'], [2, 'Today']]} format={(v) => v + ' USD'} />);
    expect(screen.getByText('Sep 1')).toBeInTheDocument();
    expect(screen.getByText('Today')).toBeInTheDocument();
    expect(screen.getByText('0 USD', { selector: 'text' })).toBeInTheDocument();
  });

  it('copes with no data', () => {
    const { container } = render(<DayBars values={[]} />);
    expect(container.querySelectorAll('rect')).toHaveLength(0);
  });
});
