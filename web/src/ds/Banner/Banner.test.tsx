import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import { Banner } from './Banner';

describe('Banner', () => {
  it('announces danger and warning as alerts and the rest as status', () => {
    const { rerender } = render(<Banner tone="danger" title="Couldn't reach the model provider." />);
    expect(screen.getByRole('alert')).toHaveTextContent("Couldn't reach the model provider.");
    rerender(<Banner tone="warning" title="3 messages need you." />);
    expect(screen.getByRole('alert')).toBeInTheDocument();
    rerender(<Banner tone="success" title="Backup restored." />);
    expect(screen.getByRole('status')).toHaveTextContent('Backup restored.');
  });

  it('shows the sentence, action and dismiss button', async () => {
    const onDismiss = vi.fn();
    render(
      <Banner title="Weather service unreachable" action={<button>Retry now</button>} onDismiss={onDismiss}>
        Retrying in 5 minutes.
      </Banner>,
    );
    expect(screen.getByText('Retrying in 5 minutes.')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Retry now' })).toBeInTheDocument();
    await userEvent.click(screen.getByRole('button', { name: 'Dismiss' }));
    expect(onDismiss).toHaveBeenCalledTimes(1);
  });
});
