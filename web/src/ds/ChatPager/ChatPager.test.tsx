import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import { ChatPager } from './ChatPager';

describe('ChatPager', () => {
  it('shows the title and position', () => {
    render(<ChatPager title="Past chat · 12 to 26 Sept" index={3} total={7} />);
    expect(screen.getByText('Past chat · 12 to 26 Sept')).toBeInTheDocument();
    expect(screen.getByText('3 of 7')).toBeInTheDocument();
  });

  it('steps older, newer and back to the current chat', async () => {
    const onOlder = vi.fn();
    const onNewer = vi.fn();
    const onCurrent = vi.fn();
    render(<ChatPager title="T" index={3} total={7} onOlder={onOlder} onNewer={onNewer} onCurrent={onCurrent} />);
    await userEvent.click(screen.getByRole('button', { name: 'Older chat' }));
    await userEvent.click(screen.getByRole('button', { name: 'Newer chat' }));
    await userEvent.click(screen.getByRole('button', { name: 'Current chat' }));
    expect(onOlder).toHaveBeenCalledTimes(1);
    expect(onNewer).toHaveBeenCalledTimes(1);
    expect(onCurrent).toHaveBeenCalledTimes(1);
  });

  it('disables Older on the first chat and Newer on the last', () => {
    const { rerender } = render(<ChatPager title="T" index={1} total={7} />);
    expect(screen.getByRole('button', { name: 'Older chat' })).toBeDisabled();
    expect(screen.getByRole('button', { name: 'Newer chat' })).toBeEnabled();
    rerender(<ChatPager title="T" index={7} total={7} />);
    expect(screen.getByRole('button', { name: 'Newer chat' })).toBeDisabled();
  });

  it('has no Current chat button without onCurrent', () => {
    render(<ChatPager title="T" index={2} total={7} />);
    expect(screen.queryByRole('button', { name: 'Current chat' })).not.toBeInTheDocument();
  });
});
