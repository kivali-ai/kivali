import type { ReactNode } from 'react';
import * as RTabs from '@radix-ui/react-tabs';
import { cx } from '../cx';

export interface TabSpec {
  value: string;
  label: string;
  count?: number;
  content?: ReactNode;
}

export interface TabsProps {
  tabs: TabSpec[];
  value?: string;
  defaultValue?: string;
  onValueChange?(v: string): void;
  className?: string;
}

/**
 * Switches between views of one thing (an agent's chat, memory, role and past chats). Built on Radix Tabs: arrow keys move between tabs.
 *
 * - `tabs` is a list of `{value, label, count?, content?}`; `count` shows a small neutral number. Control with `value` and `onValueChange`, or let it manage itself.
 * - The active tab is ink with a 2px underline; the rest are ink-muted. Keep labels to one or two words, sentence case.
 * - A tab without `content` renders no panel and its trigger names none in `aria-controls`; give the view the page renders for it `role="tabpanel"` and the trigger's id yourself if it should be announced as one.
 * - Tabs switch views of the same object. For moving between different places, use `NavItem`.
 */
export function Tabs({ tabs = [], value, defaultValue, onValueChange, className }: TabsProps) {
  const first = tabs[0]?.value;
  return (
    <RTabs.Root
      className={cx('kv-tabs', className)}
      value={value}
      defaultValue={value === undefined ? (defaultValue ?? first) : undefined}
      onValueChange={onValueChange}
    >
      <RTabs.List className="kv-tabs-list">
        {tabs.map((t) => (
          // A tab with no content has no panel (the page renders it elsewhere), so it points at none.
          <RTabs.Trigger key={t.value} value={t.value} className="kv-tab" {...(t.content == null ? { 'aria-controls': undefined } : {})}>
            {t.label}
            {t.count != null && <span className="kv-tab-count">{t.count}</span>}
          </RTabs.Trigger>
        ))}
      </RTabs.List>
      {tabs.map(
        (t) =>
          t.content != null && (
            <RTabs.Content key={t.value} value={t.value} className="kv-tabs-panel">
              {t.content}
            </RTabs.Content>
          ),
      )}
    </RTabs.Root>
  );
}
