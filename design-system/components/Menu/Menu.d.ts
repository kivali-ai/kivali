import * as React from 'react';
/**
 * Props for Menu.
 */
export interface MenuProps {
  trigger: React.ReactElement;
  items: ({ label: string;
  icon?: string;
  shortcut?: string;
  danger?: boolean;
  disabled?: boolean;
  /** Rendered at the row's right edge, e.g. a check on the current choice. */
  right?: React.ReactNode;
  onSelect?(): void } | { separator: true })[];
  align?: 'start' | 'center' | 'end';
  /** Which side of the trigger the menu opens on; the sidebar account menu opens `top`. */
  side?: 'top' | 'right' | 'bottom' | 'left';
  defaultOpen?: boolean }
export declare function Menu(props: MenuProps): JSX.Element;
export declare function usePopover(defaultOpen?: boolean): { open: boolean; setOpen(v: boolean): void; ref: React.RefObject<HTMLElement> };
export declare function MenuItem(p: { icon?: string; label?: string; shortcut?: string; right?: React.ReactNode; danger?: boolean; disabled?: boolean; onSelect?(): void; close?(): void; children?: React.ReactNode }): JSX.Element;
