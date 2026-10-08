import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import { Composer } from './Composer';

describe('Composer', () => {
  it('sends on Enter and clears the field', async () => {
    const onSend = vi.fn();
    render(<Composer placeholder="Message Garden advisor" onSend={onSend} />);
    const box = screen.getByRole('textbox', { name: 'Message Garden advisor' });
    await userEvent.type(box, 'Water the beds{Enter}');
    expect(onSend).toHaveBeenCalledWith('Water the beds');
    expect(box).toHaveValue('');
  });

  it('adds a newline on Shift+Enter without sending', async () => {
    const onSend = vi.fn();
    render(<Composer placeholder="Message" onSend={onSend} />);
    const box = screen.getByRole('textbox');
    await userEvent.type(box, 'one{Shift>}{Enter}{/Shift}two');
    expect(onSend).not.toHaveBeenCalled();
    expect(box).toHaveValue('one\ntwo');
  });

  it('does not send an empty message', async () => {
    const onSend = vi.fn();
    render(<Composer onSend={onSend} />);
    expect(screen.getByRole('button', { name: 'Send' })).toBeDisabled();
    await userEvent.type(screen.getByRole('textbox'), '   {Enter}');
    expect(onSend).not.toHaveBeenCalled();
  });

  it('sends blank text only when the consumer allows it (attachments staged)', async () => {
    const onSend = vi.fn();
    const { rerender } = render(<Composer onSend={onSend} allowEmpty />);
    expect(screen.getByRole('button', { name: 'Send' })).toBeEnabled();
    await userEvent.type(screen.getByRole('textbox'), '{Enter}');
    expect(onSend).toHaveBeenCalledWith('');
    rerender(<Composer onSend={onSend} />);
    expect(screen.getByRole('button', { name: 'Send' })).toBeDisabled();
  });

  it('blocks sending while busy', async () => {
    const onSend = vi.fn();
    render(<Composer defaultValue="Working on it" busy onSend={onSend} />);
    expect(screen.getByRole('button', { name: 'Working' })).toBeDisabled();
    await userEvent.type(screen.getByRole('textbox'), '{Enter}');
    expect(onSend).not.toHaveBeenCalled();
  });

  it('disables the field when disabled', () => {
    render(<Composer disabled />);
    expect(screen.getByRole('textbox')).toBeDisabled();
  });

  it('resizes its text area without crashing in jsdom', async () => {
    render(<Composer />);
    const box = screen.getByRole('textbox');
    await userEvent.type(box, 'a{Shift>}{Enter}{/Shift}b{Shift>}{Enter}{/Shift}c');
    expect(box).toHaveValue('a\nb\nc');
    expect(box.style.height).not.toBe('');
  });

  it('follows a controlled value and reports edits', async () => {
    const onChange = vi.fn();
    render(<Composer value="fixed" onChange={onChange} />);
    const box = screen.getByRole('textbox');
    expect(box).toHaveValue('fixed');
    await userEvent.type(box, 'x');
    expect(onChange).toHaveBeenCalledWith('fixedx');
  });

  it('shows the attach button and footer when given', async () => {
    const onAttach = vi.fn();
    render(<Composer onAttach={onAttach} footer={<span>sonnet · high</span>} />);
    await userEvent.click(screen.getByRole('button', { name: 'Attach files' }));
    expect(onAttach).toHaveBeenCalledTimes(1);
    expect(screen.getByText('sonnet · high')).toBeInTheDocument();
  });
});
