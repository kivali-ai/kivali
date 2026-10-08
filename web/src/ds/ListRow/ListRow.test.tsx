import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import { ListRow } from './ListRow';

describe('ListRow', () => {
  it('renders title, meta and trail', () => {
    render(<ListRow title="Chief of Staff" meta="Reports to you" trail="2m ago" lead={<span>lead</span>} />);
    expect(screen.getByText('Chief of Staff')).toBeInTheDocument();
    expect(screen.getByText('Reports to you')).toBeInTheDocument();
    expect(screen.getByText('2m ago')).toBeInTheDocument();
  });

  it('is a button when it has onClick', async () => {
    const onClick = vi.fn();
    render(<ListRow title="Garden advisor" onClick={onClick} selected />);
    const row = screen.getByRole('button', { name: 'Garden advisor' });
    expect(row).toHaveClass('is-selected');
    await userEvent.click(row);
    expect(onClick).toHaveBeenCalledTimes(1);
  });

  it('says whether it is expanded only when told', () => {
    const { rerender } = render(<ListRow title="Approval" onClick={() => {}} />);
    expect(screen.getByRole('button', { name: 'Approval' })).not.toHaveAttribute('aria-expanded');
    rerender(<ListRow title="Approval" onClick={() => {}} expanded={false} />);
    expect(screen.getByRole('button', { name: 'Approval' })).toHaveAttribute('aria-expanded', 'false');
    rerender(<ListRow title="Approval" onClick={() => {}} expanded />);
    expect(screen.getByRole('button', { name: 'Approval' })).toHaveAttribute('aria-expanded', 'true');
  });

  it('is a link when it has href', () => {
    render(<ListRow title="Release memo" href="/files/1" />);
    expect(screen.getByRole('link', { name: 'Release memo' })).toHaveAttribute('href', '/files/1');
  });

  it('is plain content otherwise', () => {
    render(<ListRow title="Static" />);
    expect(screen.queryByRole('button')).not.toBeInTheDocument();
    expect(screen.queryByRole('link')).not.toBeInTheDocument();
  });
});
