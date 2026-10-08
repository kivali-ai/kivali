import { useEffect, useRef, useState } from 'react';
import type { ReactElement, ReactNode, RefObject } from 'react';
import * as RMenu from '@radix-ui/react-dropdown-menu';
import { cx } from '../cx';
import { Icon } from '../Icon/Icon';

/**
 * Open state for a hand-built popover: closes on a mousedown outside `ref` and on Escape.
 * `Menu` uses Radix instead; this is for small custom disclosures that are not menus.
 */
export function usePopover<T extends HTMLElement = HTMLElement>(defaultOpen = false): { open: boolean; setOpen(v: boolean): void; ref: RefObject<T | null> } {
  const [open, setOpen] = useState(defaultOpen);
  const ref = useRef<T>(null);
  useEffect(() => {
    if (!open) return;
    const down = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false);
    };
    const key = (e: KeyboardEvent) => {
      if (e.key === 'Escape') setOpen(false);
    };
    document.addEventListener('mousedown', down);
    document.addEventListener('keydown', key);
    return () => {
      document.removeEventListener('mousedown', down);
      document.removeEventListener('keydown', key);
    };
  }, [open]);
  return { open, setOpen, ref };
}

export interface MenuItemProps {
  icon?: string;
  label?: string;
  shortcut?: string;
  right?: ReactNode;
  danger?: boolean;
  disabled?: boolean;
  onSelect?(): void;
  /** Called after `onSelect`, for any extra work on select. Radix closes the menu by itself. */
  close?(): void;
  children?: ReactNode;
}

/**
 * One row of a menu. Used by `Menu`; render it directly only inside a Radix menu content.
 * Destructive items are `danger`; a disabled item never fires `onSelect`.
 */
export function MenuItem({ icon, label, shortcut, right, danger, disabled, onSelect, close, children }: MenuItemProps) {
  return (
    <RMenu.Item
      className={cx('kv-menu-item', danger && 'is-danger')}
      disabled={disabled}
      onSelect={() => {
        onSelect?.();
        close?.();
      }}
    >
      {children ?? (
        <>
          {icon && <Icon name={icon} />}
          <span>{label}</span>
          {shortcut && <span className="kv-menu-right kv-menu-kbd">{shortcut}</span>}
          {right}
        </>
      )}
    </RMenu.Item>
  );
}

export type MenuEntry =
  | {
      label: string;
      icon?: string;
      shortcut?: string;
      /** Rendered at the row's right edge (wrap it in `.kv-menu-right`), e.g. the check on the current choice. MenuItem already renders it. */
      right?: ReactNode;
      danger?: boolean;
      disabled?: boolean;
      onSelect?(): void;
    }
  | { separator: true };

export interface MenuProps {
  trigger: ReactElement;
  items: MenuEntry[];
  align?: 'start' | 'center' | 'end';
  /** Which side of the trigger the menu opens on; the account menu at the foot of the sidebar opens `top`. */
  side?: 'top' | 'right' | 'bottom' | 'left';
  defaultOpen?: boolean;
}

/**
 * A short list of actions behind a button (usually an icon-only `ghost` button with `ellipsis`). Built on Radix Dropdown Menu: keyboard, typeahead and focus are handled.
 *
 * - `trigger` is the element that opens it; `items` are `{label, icon?, shortcut?, danger?, disabled?, onSelect}` or `{separator: true}`.
 * - Destructive items are `danger`, sit last after a separator, and open a confirm `Dialog` rather than acting at once.
 * - Keep menus under about eight items; group with separators, not headings.
 */
export function Menu({ trigger, items = [], align = 'end', side = 'bottom', defaultOpen }: MenuProps) {
  return (
    <RMenu.Root defaultOpen={defaultOpen}>
      <RMenu.Trigger asChild>{trigger}</RMenu.Trigger>
      <RMenu.Portal>
        <RMenu.Content className="kv-menu" align={align} side={side} sideOffset={6}>
          {items.map((it, i) =>
            'separator' in it ? <RMenu.Separator key={i} className="kv-menu-sep" /> : <MenuItem key={i} {...it} />,
          )}
        </RMenu.Content>
      </RMenu.Portal>
    </RMenu.Root>
  );
}
