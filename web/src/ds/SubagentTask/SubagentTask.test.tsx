import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { SubagentTask } from './SubagentTask';

describe('SubagentTask', () => {
  it('shows what was delegated, the model pill and elapsed time', () => {
    render(<SubagentTask title="Compare three suppliers" model="sonnet" effort="medium" elapsed="1m 12s" />);
    expect(screen.getByText('Compare three suppliers')).toBeInTheDocument();
    expect(screen.getByText('sonnet · medium')).toBeInTheDocument();
    expect(screen.getByText('1m 12s')).toBeInTheDocument();
  });

  it('shows the latest step only while running', () => {
    const { rerender } = render(<SubagentTask title="T" state="running" latest="Reading page 4 of 9" />);
    expect(screen.getByText('Reading page 4 of 9')).toBeInTheDocument();
    rerender(<SubagentTask title="T" state="done" latest="Reading page 4 of 9" />);
    expect(screen.queryByText('Reading page 4 of 9')).not.toBeInTheDocument();
  });

  it('says Failed and shows the error when it errored', () => {
    render(<SubagentTask title="Pull invoices" state="errored" error="Login rejected." />);
    expect(screen.getByRole('status')).toHaveTextContent('Failed');
    expect(screen.getByText('Login rejected.')).toBeInTheDocument();
  });

  it('shows the result and a link to the full session', () => {
    render(<SubagentTask title="Rainfall" state="done" result="14mm across three days." sessionHref="/agents/x/sessions/1" />);
    expect(screen.getByText('14mm across three days.')).toBeInTheDocument();
    expect(screen.getByRole('link', { name: /Open full session/ })).toHaveAttribute('href', '/agents/x/sessions/1');
  });

  it('starts collapsed and can start open', () => {
    const { container, rerender } = render(<SubagentTask title="T" />);
    expect(container.querySelector('details')).not.toHaveAttribute('open');
    rerender(<SubagentTask title="T" defaultOpen />);
    expect(container.querySelector('details')).toHaveAttribute('open');
  });
});
