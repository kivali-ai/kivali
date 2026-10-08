import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import { Button } from '../Button/Button';
import { Icon } from '../Icon/Icon';
import { Menu, usePopover } from './Menu';

function PopoverProbe() {
  const p = usePopover<HTMLDivElement>(true);
  return (
    <div>
      <div ref={p.ref}>{p.open ? 'open' : 'closed'}</div>
      <span>outside</span>
    </div>
  );
}

describe('usePopover', () => {
  it('closes on an outside mousedown', async () => {
    render(<PopoverProbe />);
    await userEvent.click(screen.getByText('open'));
    expect(screen.getByText('open')).toBeInTheDocument();
    await userEvent.click(screen.getByText('outside'));
    expect(screen.getByText('closed')).toBeInTheDocument();
  });

  it('closes on Escape', async () => {
    render(<PopoverProbe />);
    await userEvent.keyboard('{Escape}');
    expect(screen.getByText('closed')).toBeInTheDocument();
  });
});

function Sample({ onEdit, onLocked, onOffboard }: { onEdit?: () => void; onLocked?: () => void; onOffboard?: () => void }) {
  return (
    <Menu
      trigger={<Button variant="ghost" iconOnly icon={<Icon name="ellipsis" />} aria-label="Actions" />}
      items={[
        { icon: 'pencil', label: 'Edit brief', onSelect: onEdit },
        { label: 'Rotate keys', disabled: true, onSelect: onLocked },
        { separator: true },
        { icon: 'archive', label: 'Offboard', danger: true, onSelect: onOffboard },
      ]}
    />
  );
}

describe('Menu', () => {
  it('opens from its trigger and lists items with a separator', async () => {
    render(<Sample />);
    const trigger = screen.getByRole('button', { name: 'Actions' });
    expect(trigger).toHaveAttribute('aria-haspopup', 'menu');
    expect(screen.queryByRole('menu')).not.toBeInTheDocument();
    await userEvent.click(trigger);
    expect(screen.getByRole('menu')).toBeInTheDocument();
    expect(screen.getAllByRole('menuitem')).toHaveLength(3);
    expect(screen.getByRole('separator')).toBeInTheDocument();
  });

  it('calls onSelect for the chosen item and closes', async () => {
    const onEdit = vi.fn();
    render(<Sample onEdit={onEdit} />);
    await userEvent.click(screen.getByRole('button', { name: 'Actions' }));
    await userEvent.click(screen.getByRole('menuitem', { name: 'Edit brief' }));
    expect(onEdit).toHaveBeenCalledTimes(1);
    expect(screen.queryByRole('menu')).not.toBeInTheDocument();
  });

  it('marks a danger item and calls its handler', async () => {
    const onOffboard = vi.fn();
    render(<Sample onOffboard={onOffboard} />);
    await userEvent.click(screen.getByRole('button', { name: 'Actions' }));
    const item = screen.getByRole('menuitem', { name: 'Offboard' });
    expect(item).toHaveClass('is-danger');
    await userEvent.click(item);
    expect(onOffboard).toHaveBeenCalledTimes(1);
  });

  it('does not select a disabled item', async () => {
    const onLocked = vi.fn();
    render(<Sample onLocked={onLocked} />);
    await userEvent.click(screen.getByRole('button', { name: 'Actions' }));
    const item = screen.getByRole('menuitem', { name: 'Rotate keys' });
    expect(item).toHaveAttribute('aria-disabled', 'true');
    await userEvent.click(item);
    expect(onLocked).not.toHaveBeenCalled();
  });

  it('closes on Escape', async () => {
    render(<Sample />);
    await userEvent.click(screen.getByRole('button', { name: 'Actions' }));
    await userEvent.keyboard('{Escape}');
    expect(screen.queryByRole('menu')).not.toBeInTheDocument();
  });

  it('accepts a side so the account menu can open upward', async () => {
    render(
      <Menu side="top" trigger={<Button>Account</Button>} items={[{ label: 'Sign out' }]} />,
    );
    await userEvent.click(screen.getByRole('button', { name: 'Account' }));
    expect(screen.getByRole('menu')).toHaveAttribute('data-side', 'top');
  });
});
