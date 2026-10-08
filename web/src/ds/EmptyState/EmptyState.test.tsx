import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { EmptyState } from './EmptyState';

describe('EmptyState', () => {
  it('renders the title, sentence and action', () => {
    render(
      <EmptyState title="No agents yet" action={<button>Hire your first agent</button>}>
        Every team starts with one.
      </EmptyState>,
    );
    expect(screen.getByRole('heading', { name: 'No agents yet' })).toBeInTheDocument();
    expect(screen.getByText('Every team starts with one.')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Hire your first agent' })).toBeInTheDocument();
  });

  it('draws the brand dot grid with two honey dots', () => {
    const { container } = render(<EmptyState title="Inbox zero" />);
    expect(container.querySelectorAll('.kv-empty-honey')).toHaveLength(2);
    expect(container.querySelectorAll('.kv-empty-dot')).toHaveLength(38);
  });
});
