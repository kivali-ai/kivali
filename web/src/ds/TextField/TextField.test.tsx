import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import { TextField } from './TextField';

describe('TextField', () => {
  it('labels the input and links the hint', () => {
    render(<TextField label="Agent name" hint="Shown on the org chart." />);
    const input = screen.getByRole('textbox', { name: 'Agent name' });
    expect(input).toHaveAccessibleDescription('Shown on the org chart.');
  });

  it('shows an error in place of the hint and marks the input invalid', () => {
    render(<TextField label="Brief" hint="Hint" error="Keep briefs under 500 characters." />);
    const input = screen.getByRole('textbox', { name: 'Brief' });
    expect(input).toBeInvalid();
    expect(input).toHaveAccessibleDescription('Keep briefs under 500 characters.');
    expect(screen.queryByText('Hint')).not.toBeInTheDocument();
  });

  it('renders a textarea when multiline', () => {
    render(<TextField label="Brief" multiline rows={3} defaultValue="Check the inbox." />);
    const box = screen.getByRole('textbox', { name: 'Brief' });
    expect(box.tagName).toBe('TEXTAREA');
    expect(box).toHaveAttribute('rows', '3');
  });

  it('passes input props through', async () => {
    const onChange = vi.fn();
    render(<TextField label="Name" name="name" onChange={onChange} />);
    await userEvent.type(screen.getByRole('textbox', { name: 'Name' }), 'a');
    expect(onChange).toHaveBeenCalledTimes(1);
  });
});
