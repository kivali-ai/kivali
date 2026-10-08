import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import { NavItem } from './NavItem';

describe('NavItem', () => {
  it('is a button with a label, count and current-page marker', async () => {
    const onClick = vi.fn();
    render(<NavItem icon="inbox" label="Inbox" count={3} attention active onClick={onClick} />);
    const item = screen.getByRole('button', { name: /Inbox/ });
    expect(item).toHaveAttribute('aria-current', 'page');
    expect(screen.getByText('3')).toHaveClass('is-attention');
    await userEvent.click(item);
    expect(onClick).toHaveBeenCalledTimes(1);
  });

  it('is a link when it has href', () => {
    render(<NavItem icon="settings" label="Settings" href="/org" />);
    expect(screen.getByRole('link', { name: 'Settings' })).toHaveAttribute('href', '/org');
  });

  it('hides a zero count and indents by depth', () => {
    render(<NavItem label="Garden advisor" count={0} depth={2} />);
    expect(screen.queryByText('0')).not.toBeInTheDocument();
    expect(screen.getByRole('button').style.paddingLeft).toBe('42px');
  });
});
