import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import { QueueRow } from './QueueRow';

const from = { name: 'Chief of Staff', role: 'compass', color: 'iris' };
const to = [{ name: 'Supplier scout', role: 'search', color: 'clay' }];

describe('QueueRow', () => {
  it('shows the route, title and the countdown', () => {
    render(<QueueRow from={from} to={to} kind="assignment" refId={12} title="Price out mail providers" releasesIn={24} />);
    expect(screen.getByText('Price out mail providers')).toBeInTheDocument();
    expect(screen.getByText('Chief of Staff → Supplier scout', { exact: false })).toBeInTheDocument();
    expect(screen.getByText('assignment #12')).toBeInTheDocument();
    expect(screen.getByText('Releases in 0:24')).toBeInTheDocument();
  });

  it('says Held with auto-release off and Releasing at zero', () => {
    const { rerender } = render(<QueueRow from={from} to={to} title="T" held />);
    expect(screen.getByText('Held')).toBeInTheDocument();
    rerender(<QueueRow from={from} to={to} title="T" releasesIn={0} />);
    expect(screen.getByText('Releasing')).toBeInTheDocument();
  });

  it('marks a notice', () => {
    render(<QueueRow from={from} to={to} title="T" />);
    expect(screen.getByText('notice')).toBeInTheDocument();
  });

  it('releases in one step', async () => {
    const onRelease = vi.fn();
    render(<QueueRow from={from} to={to} title="T" onRelease={onRelease} />);
    await userEvent.click(screen.getByRole('button', { name: 'Release' }));
    expect(onRelease).toHaveBeenCalledTimes(1);
  });

  it('counts extra recipients', () => {
    render(<QueueRow from={from} to={[...to, { name: 'Bookkeeper' }, { name: 'Garden advisor' }]} title="T" />);
    expect(screen.getByText('+2')).toBeInTheDocument();
  });

  it('expands to show its body', async () => {
    const onToggle = vi.fn();
    const { rerender } = render(
      <QueueRow from={from} to={to} title="T" onToggle={onToggle}>
        <p>Body</p>
      </QueueRow>,
    );
    expect(screen.queryByText('Body')).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole('button', { name: /T/, expanded: false }));
    expect(onToggle).toHaveBeenCalledTimes(1);
    rerender(
      <QueueRow from={from} to={to} title="T" expanded onToggle={onToggle}>
        <p>Body</p>
      </QueueRow>,
    );
    expect(screen.getByText('Body')).toBeInTheDocument();
  });

  it('shows a selection checkbox when selectable', async () => {
    const onSelect = vi.fn();
    render(<QueueRow from={from} to={to} title="T" selectable selected={false} onSelect={onSelect} />);
    await userEvent.click(screen.getByRole('checkbox', { name: 'Select message' }));
    expect(onSelect).toHaveBeenCalledWith(true);
  });
});
