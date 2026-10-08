import { useState } from 'react';
import { Button, Icon, Menu } from '../ds';
import type { MenuEntry } from '../ds';
import { applyTheme, getStoredTheme } from './theme';
import type { ThemeChoice } from './theme';

const THEMES: ReadonlyArray<{ choice: ThemeChoice; label: string; icon: string }> = [
  { choice: 'device', label: 'Match device', icon: 'laptop' },
  { choice: 'light', label: 'Light', icon: 'sun' },
  { choice: 'dark', label: 'Dark', icon: 'moon' },
];

export interface AccountMenuProps {
  onSignOut(): void;
}

/** The person's menu at the sidebar foot. It opens upward: theme choice, then Sign out. */
export function AccountMenu({ onSignOut }: AccountMenuProps) {
  const [theme, setTheme] = useState<ThemeChoice>(getStoredTheme);

  // The reference marks the current theme with a check at the right edge.
  const themeItems: MenuEntry[] = THEMES.map((t) => ({
    label: t.label,
    icon: t.icon,
    onSelect: () => {
      applyTheme(t.choice);
      setTheme(t.choice);
    },
    right:
      t.choice === theme ? (
        <span className="kv-menu-right">
          <Icon name="check" label="Current" />
        </span>
      ) : undefined,
  }));

  const items: MenuEntry[] = [...themeItems, { separator: true }, { label: 'Sign out', icon: 'log-out', onSelect: onSignOut }];

  return (
    <Menu
      side="top"
      align="end"
      items={items}
      trigger={<Button variant="ghost" iconOnly icon={<Icon name="ellipsis" />} aria-label="Account" />}
    />
  );
}
