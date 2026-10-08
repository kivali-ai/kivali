import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { useState } from 'react';
import { describe, expect, it, vi } from 'vitest';
import { Checkbox } from './Checkbox';

describe('Checkbox', () => {
  it('renders a labelled checkbox with a hint', () => {
    render(<Checkbox label="Include past chats" hint="Adds archived conversations." defaultChecked />);
    expect(screen.getByRole('checkbox', { name: /Include past chats/ })).toBeChecked();
    expect(screen.getByText('Adds archived conversations.')).toBeInTheDocument();
  });

  it('shows the indeterminate state as mixed and a click makes it checked', async () => {
    const onCheckedChange = vi.fn();
    render(<Checkbox label="Select all" checked="indeterminate" onCheckedChange={onCheckedChange} />);
    const box = screen.getByRole('checkbox', { name: 'Select all' });
    expect(box).toHaveAttribute('aria-checked', 'mixed');
    expect(box).toHaveAttribute('data-state', 'indeterminate');
    await userEvent.click(box);
    expect(onCheckedChange).toHaveBeenCalledWith(true);
  });

  it('reports changes and follows a controlled value', async () => {
    const seen: Array<boolean | 'indeterminate'> = [];
    function Controlled() {
      const [v, setV] = useState<boolean | 'indeterminate'>(false);
      return (
        <Checkbox
          label="Auto-release"
          checked={v}
          onCheckedChange={(n) => {
            seen.push(n);
            setV(n);
          }}
        />
      );
    }
    render(<Controlled />);
    const box = screen.getByRole('checkbox', { name: 'Auto-release' });
    expect(box).not.toBeChecked();
    await userEvent.click(box);
    expect(box).toBeChecked();
    await userEvent.click(box);
    expect(box).not.toBeChecked();
    expect(seen).toEqual([true, false]);
  });

  it('does not change when disabled', async () => {
    const onCheckedChange = vi.fn();
    render(<Checkbox label="Locked" disabled onCheckedChange={onCheckedChange} />);
    await userEvent.click(screen.getByRole('checkbox', { name: 'Locked' }));
    expect(onCheckedChange).not.toHaveBeenCalled();
  });

  it('takes an accessible name from ariaLabel when the label is empty', () => {
    render(<Checkbox label="" ariaLabel="Select message" />);
    expect(screen.getByRole('checkbox', { name: 'Select message' })).toBeInTheDocument();
  });
});
