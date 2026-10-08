import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import { Switch } from './Switch';

describe('Switch', () => {
  it('renders a labelled switch', () => {
    render(<Switch label="Research skill" hint="Available to every agent." defaultChecked />);
    expect(screen.getByRole('switch', { name: /Research skill/ })).toBeChecked();
    expect(screen.getByText('Available to every agent.')).toBeInTheDocument();
  });

  it('toggles and reports the new value', async () => {
    const onCheckedChange = vi.fn();
    render(<Switch label="Auto-release" onCheckedChange={onCheckedChange} />);
    const sw = screen.getByRole('switch', { name: 'Auto-release' });
    expect(sw).toHaveAttribute('aria-checked', 'false');
    await userEvent.click(sw);
    expect(sw).toHaveAttribute('aria-checked', 'true');
    expect(sw).toHaveAttribute('data-state', 'checked');
    expect(onCheckedChange).toHaveBeenLastCalledWith(true);
    await userEvent.click(screen.getByText('Auto-release'));
    expect(onCheckedChange).toHaveBeenLastCalledWith(false);
  });

  it('does not toggle when disabled', async () => {
    const onCheckedChange = vi.fn();
    render(<Switch label="Locked" disabled onCheckedChange={onCheckedChange} />);
    await userEvent.click(screen.getByRole('switch', { name: 'Locked' }));
    expect(onCheckedChange).not.toHaveBeenCalled();
  });
});
