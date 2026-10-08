import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { NodeChip } from './NodeChip';

describe('NodeChip', () => {
  it('shows the id and summary', () => {
    render(<NodeChip id="decision/mail-provider" kind="decision" summary="Use the new mail provider" />);
    expect(screen.getByText('decision/mail-provider')).toBeInTheDocument();
    expect(screen.getByText('Use the new mail provider')).toBeInTheDocument();
  });

  it('is a link with href and carries the owner tooltip', () => {
    const { container } = render(<NodeChip id="n" href="/graph/n" owner="iris" ownerName="Chief of Staff" />);
    expect(screen.getByRole('link')).toHaveAttribute('href', '/graph/n');
    expect(container.querySelector('.kv-node-owner')).toHaveAttribute('title', 'Chief of Staff');
  });

  it('flags a flagged node and an unresolved link with named icons', () => {
    const { rerender } = render(<NodeChip id="req/x" status="flagged" />);
    expect(screen.getByRole('img', { name: 'Flagged' })).toBeInTheDocument();
    rerender(<NodeChip id="req/x" status="unresolved" />);
    expect(screen.getByRole('img', { name: 'Unresolved link' })).toBeInTheDocument();
  });

  it('puts the status in the tooltip when not active', () => {
    render(<NodeChip id="decision/old" status="superseded" />);
    expect(screen.getByTitle('decision/old · superseded')).toBeInTheDocument();
  });

  it('marks selection', () => {
    render(<NodeChip id="x" selected />);
    expect(screen.getByTitle('x')).toHaveClass('is-selected');
  });
});
