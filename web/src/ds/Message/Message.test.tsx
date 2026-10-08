import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import { Message } from './Message';

const agent = { kind: 'agent' as const, name: 'Garden advisor', role: 'sprout', color: 'olive' };
const person = { kind: 'person' as const, name: 'Maya Chen' };

describe('Message', () => {
  it('renders an agent reply with its name, time and body', () => {
    render(<Message from={agent} time="6:02">Bed 3 is dry.</Message>);
    expect(screen.getByText('Garden advisor')).toBeInTheDocument();
    expect(screen.getByText('6:02')).toBeInTheDocument();
    expect(screen.getByText('Bed 3 is dry.')).toBeInTheDocument();
  });

  it('shows the model badge for an agent only', () => {
    const { rerender } = render(<Message from={agent} model="opus · high">Hello</Message>);
    expect(screen.getByText('opus · high')).toBeInTheDocument();
    rerender(<Message from={person} model="opus · high">Hello</Message>);
    expect(screen.queryByText('opus · high')).not.toBeInTheDocument();
  });

  it('renders a system message as a note', () => {
    render(<Message from={{ kind: 'system' }} time="6:30">Chat rotated</Message>);
    expect(screen.getByRole('note')).toHaveTextContent('Chat rotated');
  });

  it('marks a pending message with Delete and Send now', async () => {
    const onDelete = vi.fn();
    const onSendNow = vi.fn();
    render(
      <Message from={person} pending onDelete={onDelete} onSendNow={onSendNow}>
        Also check bed 3.
      </Message>,
    );
    expect(screen.getByText('Pending')).toBeInTheDocument();
    expect(screen.getByText('Waiting for a moment to deliver')).toBeInTheDocument();
    await userEvent.click(screen.getByRole('button', { name: 'Delete' }));
    await userEvent.click(screen.getByRole('button', { name: 'Send now' }));
    expect(onDelete).toHaveBeenCalledTimes(1);
    expect(onSendNow).toHaveBeenCalledTimes(1);
  });

  it('omits Delete when the message can no longer be taken back', () => {
    render(
      <Message from={person} pending onSendNow={() => {}}>
        Wait
      </Message>,
    );
    expect(screen.queryByRole('button', { name: 'Delete' })).toBeNull();
    expect(screen.getByRole('button', { name: 'Send now' })).toBeInTheDocument();
  });

  it('shows Send now loading until the delivery lands, and holds Delete meanwhile', () => {
    render(
      <Message from={person} pending onDelete={() => {}} onSendNow={() => {}} sendNowLoading>
        Wait
      </Message>,
    );
    expect(screen.getByRole('button', { name: 'Send now' })).toHaveAttribute('aria-busy', 'true');
    expect(screen.getByRole('button', { name: 'Delete' })).toBeDisabled();
  });

  it('has no pending controls when the message is delivered', () => {
    render(<Message from={person}>Delivered.</Message>);
    expect(screen.queryByText('Waiting for a moment to deliver')).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Send now' })).not.toBeInTheDocument();
  });

  it('exposes the pending state as a modifier class', () => {
    const { container } = render(<Message from={person} pending>Wait</Message>);
    expect(container.firstElementChild).toHaveClass('is-pending');
  });

  it('adds the streaming caret while an agent writes', () => {
    const { container } = render(<Message from={agent} streaming>Writing</Message>);
    expect(container.firstElementChild).toHaveClass('is-streaming');
    expect(container.querySelector('.kv-caret')).not.toBeNull();
  });
});
