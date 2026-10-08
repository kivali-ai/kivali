// The six Radix ports must emit exactly the classes and attributes kivali.css targets:
// .kv-checkbox[data-state=checked|indeterminate], .kv-switch[data-state=checked] .kv-switch-thumb,
// .kv-dialog-overlay / .kv-dialog / .kv-dialog--danger, .kv-tab[data-state=active], .kv-tabs-panel,
// .kv-menu / .kv-menu-item[data-highlighted] / [data-disabled] / .is-danger / .kv-menu-sep / .kv-menu-label,
// .kv-tooltip. Portalled content must sit under <html>, where data-theme="dark" lives.
import { act, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it } from 'vitest';
import { Button } from './Button/Button';
import { Checkbox } from './Checkbox/Checkbox';
import { Dialog } from './Dialog/Dialog';
import { Menu } from './Menu/Menu';
import { Switch } from './Switch/Switch';
import { Tabs } from './Tabs/Tabs';
import { Tooltip, TooltipProvider } from './Tooltip/Tooltip';

function underDarkRoot(el: Element) {
  expect(document.documentElement.contains(el)).toBe(true);
  expect(el.closest('[data-theme="dark"]')).toBe(document.documentElement);
}

afterEach(() => {
  document.documentElement.removeAttribute('data-theme');
});

describe('Radix ports satisfy the kivali.css contract', () => {
  it('Checkbox: kv-checkbox carries data-state for unchecked, checked and indeterminate', () => {
    render(
      <>
        <Checkbox label="Off" />
        <Checkbox label="On" defaultChecked />
        <Checkbox label="Some" checked="indeterminate" />
      </>,
    );
    const off = screen.getByRole('checkbox', { name: 'Off' });
    const on = screen.getByRole('checkbox', { name: 'On' });
    const some = screen.getByRole('checkbox', { name: 'Some' });
    for (const b of [off, on, some]) {
      expect(b).toHaveClass('kv-checkbox');
      expect(b.tagName).toBe('BUTTON');
      expect(b.parentElement).toHaveClass('kv-choice');
    }
    expect(off).toHaveAttribute('data-state', 'unchecked');
    expect(off.querySelector('.kv-checkbox-ind')).toBeNull();
    expect(on).toHaveAttribute('data-state', 'checked');
    expect(on.querySelector('.kv-checkbox-ind .kv-icon')).not.toBeNull();
    expect(some).toHaveAttribute('data-state', 'indeterminate');
    expect(some.querySelector('.kv-checkbox-ind')).not.toBeNull();
  });

  it('Switch: kv-switch and its thumb carry data-state on and off', async () => {
    render(<Switch label="Auto-release" />);
    const sw = screen.getByRole('switch', { name: 'Auto-release' });
    const thumb = sw.querySelector('.kv-switch-thumb');
    expect(sw).toHaveClass('kv-switch');
    expect(sw.parentElement).toHaveClass('kv-choice', 'kv-choice--switch');
    expect(sw).toHaveAttribute('data-state', 'unchecked');
    expect(thumb).toHaveAttribute('data-state', 'unchecked');
    await userEvent.click(sw);
    expect(sw).toHaveAttribute('data-state', 'checked');
    expect(thumb).toHaveAttribute('data-state', 'checked');
  });

  it('Dialog: open overlay and content carry the kv classes and sit under the themed root', () => {
    document.documentElement.setAttribute('data-theme', 'dark');
    render(<Dialog defaultOpen tone="danger" title="Offboard Garden advisor?" description="Nothing is deleted." footer={<Button>Cancel</Button>} />);
    const dialog = screen.getByRole('dialog');
    expect(dialog).toHaveClass('kv-dialog', 'kv-dialog--danger');
    expect(dialog).toHaveAttribute('aria-modal', 'true');
    expect(dialog).toHaveAttribute('data-state', 'open');
    const overlay = document.querySelector('.kv-dialog-overlay');
    expect(overlay).not.toBeNull();
    expect(overlay).toHaveAttribute('data-state', 'open');
    expect(dialog.querySelector('.kv-dialog-head > h2.kv-dialog-title')).toHaveTextContent('Offboard Garden advisor?');
    expect(dialog.querySelector('.kv-dialog-head > button.kv-dialog-close')).toHaveAttribute('aria-label', 'Close');
    expect(dialog.querySelector(':scope > p.kv-dialog-desc')).toHaveTextContent('Nothing is deleted.');
    expect(dialog.querySelector(':scope > .kv-dialog-foot')).not.toBeNull();
    underDarkRoot(dialog);
    underDarkRoot(overlay as Element);
  });

  it('Tabs: triggers carry data-state active/inactive and only the active panel shows', async () => {
    render(
      <Tabs
        tabs={[
          { value: 'chat', label: 'Chat', content: 'Chat body' },
          { value: 'memory', label: 'Memory', count: 214, content: 'Memory body' },
        ]}
      />,
    );
    const list = screen.getByRole('tablist');
    expect(list).toHaveClass('kv-tabs-list');
    expect(list.parentElement).toHaveClass('kv-tabs');
    const chat = screen.getByRole('tab', { name: 'Chat' });
    const memory = screen.getByRole('tab', { name: /Memory/ });
    expect(chat).toHaveClass('kv-tab');
    expect(chat).toHaveAttribute('data-state', 'active');
    expect(chat).toHaveAttribute('aria-selected', 'true');
    expect(memory).toHaveAttribute('data-state', 'inactive');
    expect(memory.querySelector('.kv-tab-count')).toHaveTextContent('214');
    const panel = screen.getByRole('tabpanel');
    expect(panel).toHaveClass('kv-tabs-panel');
    expect(panel).toHaveTextContent('Chat body');
    expect(screen.queryByText('Memory body')).toBeNull();
    await userEvent.click(memory);
    expect(memory).toHaveAttribute('data-state', 'active');
    expect(chat).toHaveAttribute('data-state', 'inactive');
    expect(screen.getByRole('tabpanel')).toHaveTextContent('Memory body');
  });

  it('Menu: items carry data-highlighted, data-disabled and is-danger; the portal sits under the themed root', async () => {
    document.documentElement.setAttribute('data-theme', 'dark');
    render(
      <Menu
        defaultOpen
        trigger={<Button>Actions</Button>}
        items={[
          { label: 'Edit brief', icon: 'pencil' },
          { label: 'Rotate keys', disabled: true },
          { separator: true },
          { label: 'Offboard', danger: true },
        ]}
      />,
    );
    const menu = screen.getByRole('menu');
    expect(menu).toHaveClass('kv-menu');
    underDarkRoot(menu);
    expect(menu.querySelector('.kv-menu-sep')).toHaveAttribute('role', 'separator');
    const edit = screen.getByRole('menuitem', { name: 'Edit brief' });
    const locked = screen.getByRole('menuitem', { name: 'Rotate keys' });
    const off = screen.getByRole('menuitem', { name: 'Offboard' });
    for (const it of [edit, locked, off]) expect(it).toHaveClass('kv-menu-item');
    expect(locked).toHaveAttribute('data-disabled', '');
    expect(edit).not.toHaveAttribute('data-disabled');
    expect(off).toHaveClass('is-danger');
    expect(edit).not.toHaveAttribute('data-highlighted');
    act(() => edit.focus());
    expect(edit).toHaveAttribute('data-highlighted', '');
    await userEvent.keyboard('{ArrowDown}');
    // Keyboard skips the disabled item.
    expect(off).toHaveAttribute('data-highlighted', '');
    expect(edit).not.toHaveAttribute('data-highlighted');
    expect(locked).not.toHaveAttribute('data-highlighted');
  });

  it('Tooltip: open content carries kv-tooltip and sits under the themed root', () => {
    document.documentElement.setAttribute('data-theme', 'dark');
    render(
      <TooltipProvider>
        <Tooltip open content="Release all">
          <Button>Trigger</Button>
        </Tooltip>
      </TooltipProvider>,
    );
    const tip = document.querySelector('.kv-tooltip');
    expect(tip).not.toBeNull();
    expect(tip).toHaveTextContent('Release all');
    expect(tip).toHaveAttribute('data-side', 'top');
    underDarkRoot(tip as Element);
    // Only one styled bubble; Radix's accessible copy is visually hidden and unstyled.
    expect(document.querySelectorAll('.kv-tooltip')).toHaveLength(1);
    expect(screen.getByRole('tooltip')).toHaveTextContent('Release all');
  });
});
