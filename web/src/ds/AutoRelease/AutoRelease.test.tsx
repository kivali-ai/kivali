import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import { AUTO_RELEASE_STOPS, AutoRelease } from './AutoRelease';

describe('AutoRelease', () => {
  it('exports the six fixed stops', () => {
    expect(AUTO_RELEASE_STOPS).toEqual(['Now', '30s', '2m', '5m', '20m', 'Off']);
  });

  it('renders six stops in a radio group with the current one checked', () => {
    render(<AutoRelease value="2m" />);
    expect(screen.getByRole('radiogroup', { name: 'Auto-release' })).toBeInTheDocument();
    const radios = screen.getAllByRole('radio');
    expect(radios).toHaveLength(6);
    expect(screen.getByRole('radio', { name: '2m' })).toHaveAttribute('aria-checked', 'true');
    expect(screen.getByRole('radio', { name: '30s' })).toHaveAttribute('aria-checked', 'false');
  });

  it('reports the chosen stop', async () => {
    const onChange = vi.fn();
    render(<AutoRelease value="30s" onChange={onChange} />);
    await userEvent.click(screen.getByRole('radio', { name: 'Off' }));
    expect(onChange).toHaveBeenCalledWith('Off');
  });

  it('passes the position to the track as a CSS variable', () => {
    const { container } = render(<AutoRelease value="Off" />);
    const track = container.querySelector<HTMLElement>('.kv-autorel-track');
    expect(track?.style.getPropertyValue('--kv-autorel-frac')).toBe('1');
  });

  it('renders a native select for the select variant', async () => {
    const onChange = vi.fn();
    render(<AutoRelease value="Off" variant="select" onChange={onChange} />);
    const select = screen.getByRole('combobox', { name: 'Auto-release' });
    expect(select.tagName).toBe('SELECT');
    expect(select).toHaveValue('Off');
    expect(screen.queryByRole('radio')).not.toBeInTheDocument();
    await userEvent.selectOptions(select, '5m');
    expect(onChange).toHaveBeenCalledWith('5m');
  });
});
