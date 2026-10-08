import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { RankedBars } from './RankedBars';

const items = [
  { label: 'Bookkeeper', value: 22 },
  { label: 'Chief of Staff', value: 84 },
  { label: 'Garden advisor', value: 38 },
  { label: 'Mail sorter', value: 3 },
  { label: 'Seed librarian', value: 4 },
];

describe('RankedBars', () => {
  it('ranks the items by value', () => {
    render(<RankedBars items={items} />);
    const bars = screen.getAllByRole('progressbar');
    expect(bars).toHaveLength(5);
    expect(bars[0]).toHaveAttribute('aria-valuenow', '84');
    // Each bar is named by its row: who and how much.
    expect(bars[0]).toHaveAccessibleName('Chief of Staff $84');
    expect(screen.getByText('$84')).toBeInTheDocument();
  });

  it('folds the rest into one quiet total', () => {
    render(<RankedBars items={items} top={3} />);
    expect(screen.getAllByRole('progressbar')).toHaveLength(3);
    expect(screen.getByText('Everyone else · 2 agents')).toBeInTheDocument();
    expect(screen.getByText('$7')).toBeInTheDocument();
  });

  it('uses the singular for one agent in the rest and a custom format', () => {
    render(<RankedBars items={items.slice(0, 4)} top={3} format={(v) => v + ' USD'} />);
    expect(screen.getByText('Everyone else · 1 agent')).toBeInTheDocument();
    expect(screen.getByText('3 USD')).toBeInTheDocument();
  });
});
