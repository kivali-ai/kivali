import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import { Select } from './Select';

const options = [
  { value: 'opus', label: 'Opus' },
  { value: 'sonnet', label: 'Sonnet' },
];

describe('Select', () => {
  it('is a labelled native select with a hint', () => {
    render(<Select label="Model" hint="Applies from the next turn." options={options} defaultValue="sonnet" />);
    const select = screen.getByRole('combobox', { name: 'Model' });
    expect(select.tagName).toBe('SELECT');
    expect(select).toHaveValue('sonnet');
    expect(select).toHaveAccessibleDescription('Applies from the next turn.');
  });

  it('reports the chosen option', async () => {
    const onChange = vi.fn();
    render(<Select label="Model" options={options} onChange={onChange} />);
    await userEvent.selectOptions(screen.getByRole('combobox', { name: 'Model' }), 'sonnet');
    expect(onChange).toHaveBeenCalledTimes(1);
  });

  it('shows an error', () => {
    render(<Select label="Model" options={options} error="Pick a model." />);
    expect(screen.getByRole('combobox', { name: 'Model' })).toBeInvalid();
    expect(screen.getByText('Pick a model.')).toBeInTheDocument();
  });
});
