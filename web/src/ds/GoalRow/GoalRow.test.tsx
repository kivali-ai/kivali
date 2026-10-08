import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { GoalRow } from './GoalRow';

const workers = [
  { name: 'Supplier scout', role: 'search', color: 'clay', state: 'running' as const },
  { name: 'Bookkeeper', role: 'calculator', color: 'ochre', state: 'blocked' as const },
  { name: 'Garden advisor', role: 'sprout', color: 'olive' },
  { name: 'Weather watcher' },
];

describe('GoalRow', () => {
  it('shows the title, progress, owner and blocker in words', () => {
    render(
      <GoalRow
        title="Choose a mail provider"
        owner={{ name: 'Chief of Staff', role: 'compass', color: 'iris' }}
        done={3}
        total={7}
        blocker="Waiting on vendor quotes"
      />,
    );
    expect(screen.getByText('Choose a mail provider')).toBeInTheDocument();
    expect(screen.getByText('3 of 7')).toBeInTheDocument();
    expect(screen.getByRole('progressbar', { name: '3 of 7' })).toHaveAttribute('aria-valuenow', '3');
    expect(screen.getByText('Waiting on vendor quotes')).toBeInTheDocument();
    expect(screen.getAllByText('Chief of Staff').length).toBeGreaterThan(0);
  });

  it('collapses workers past the maximum into +N more with their names', () => {
    render(<GoalRow title="G" workers={workers} maxWorkers={2} />);
    const more = screen.getByText('+2 more');
    expect(more).toHaveAttribute('title', 'Garden advisor, Weather watcher also working');
    expect(screen.getByRole('img', { name: 'Supplier scout' })).toBeInTheDocument();
    expect(screen.queryByRole('img', { name: 'Garden advisor' })).not.toBeInTheDocument();
  });

  it('links the title when it has an href', () => {
    render(<GoalRow title="Draft the release plan" href="/work/goals/1" done={5} total={5} />);
    expect(screen.getByRole('link', { name: 'Draft the release plan' })).toHaveAttribute('href', '/work/goals/1');
  });
});
