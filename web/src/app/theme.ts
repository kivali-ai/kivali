export type ThemeChoice = 'device' | 'light' | 'dark';

export const THEME_KEY = 'kivali.theme';

const CHOICES: readonly ThemeChoice[] = ['device', 'light', 'dark'];

/** The stored choice, or 'device' when nothing valid is stored or storage is unavailable. */
export function getStoredTheme(): ThemeChoice {
  try {
    const v = window.localStorage.getItem(THEME_KEY);
    if (v && (CHOICES as readonly string[]).includes(v)) return v as ThemeChoice;
  } catch {
    // Private windows and blocked storage throw; fall through to the default.
  }
  return 'device';
}

let current: ThemeChoice = 'device';
let watching = false;

function systemPrefersDark(): boolean {
  return typeof window.matchMedia === 'function' && window.matchMedia('(prefers-color-scheme: dark)').matches;
}

function paint(choice: ThemeChoice): void {
  const dark = choice === 'dark' || (choice === 'device' && systemPrefersDark());
  const root = document.documentElement;
  if (dark) root.setAttribute('data-theme', 'dark');
  else root.removeAttribute('data-theme');
}

function watchSystem(): void {
  if (watching || typeof window.matchMedia !== 'function') return;
  watching = true;
  window.matchMedia('(prefers-color-scheme: dark)').addEventListener('change', () => {
    if (current === 'device') paint('device');
  });
}

/**
 * Sets or removes `data-theme="dark"` on `<html>` (the design system's dark theme keys off it),
 * follows the operating system while the choice is 'device', and remembers the choice.
 * Pass `persist: false` to apply without writing (startup).
 */
export function applyTheme(choice: ThemeChoice, persist = true): void {
  current = choice;
  paint(choice);
  watchSystem();
  if (!persist) return;
  try {
    window.localStorage.setItem(THEME_KEY, choice);
  } catch {
    // The choice still applies for this page view.
  }
}
