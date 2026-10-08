import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import { Toast } from './Toast';

describe('Toast', () => {
  it('announces danger as an alert and others as status', () => {
    const { rerender } = render(<Toast tone="danger" title="Couldn't save the role" />);
    expect(screen.getByRole('alert')).toHaveTextContent("Couldn't save the role");
    rerender(<Toast tone="success" title="Skill installed" />);
    expect(screen.getByRole('status')).toHaveTextContent('Skill installed');
  });

  it('shows the sentence, action and dismiss button', async () => {
    const onDismiss = vi.fn();
    render(
      <Toast title="3 messages released" action={<button>Undo</button>} onDismiss={onDismiss}>
        Delivered to two agents.
      </Toast>,
    );
    expect(screen.getByText('Delivered to two agents.')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Undo' })).toBeInTheDocument();
    await userEvent.click(screen.getByRole('button', { name: 'Dismiss' }));
    expect(onDismiss).toHaveBeenCalledTimes(1);
  });
});
