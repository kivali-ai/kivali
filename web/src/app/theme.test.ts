import { beforeEach, describe, expect, it, vi } from 'vitest';
import { THEME_KEY, applyTheme, getStoredTheme } from './theme';

function mockSystemDark(dark: boolean) {
  vi.stubGlobal('matchMedia', (q: string) => ({
    matches: dark && q.includes('dark'),
    media: q,
    addEventListener: () => {},
    removeEventListener: () => {},
  }));
  window.matchMedia = globalThis.matchMedia;
}

describe('theme', () => {
  beforeEach(() => {
    window.localStorage.clear();
    document.documentElement.removeAttribute('data-theme');
    mockSystemDark(false);
  });

  it('sets data-theme="dark" for dark and removes it for light', () => {
    applyTheme('dark');
    expect(document.documentElement).toHaveAttribute('data-theme', 'dark');
    applyTheme('light');
    expect(document.documentElement).not.toHaveAttribute('data-theme');
  });

  it('persists the choice under kivali.theme', () => {
    applyTheme('dark');
    expect(window.localStorage.getItem(THEME_KEY)).toBe('dark');
    expect(getStoredTheme()).toBe('dark');
  });

  it('follows the system preference for device', () => {
    mockSystemDark(true);
    applyTheme('device');
    expect(document.documentElement).toHaveAttribute('data-theme', 'dark');
    mockSystemDark(false);
    applyTheme('device');
    expect(document.documentElement).not.toHaveAttribute('data-theme');
  });

  it('does not persist when told not to', () => {
    applyTheme('dark', false);
    expect(window.localStorage.getItem(THEME_KEY)).toBeNull();
  });

  it('defaults to device for a missing or invalid stored value', () => {
    expect(getStoredTheme()).toBe('device');
    window.localStorage.setItem(THEME_KEY, 'purple');
    expect(getStoredTheme()).toBe('device');
  });

  it('survives storage that throws', () => {
    const real = window.localStorage;
    const blocked = {
      getItem: () => {
        throw new Error('blocked');
      },
      setItem: () => {
        throw new Error('blocked');
      },
    };
    Object.defineProperty(window, 'localStorage', { value: blocked, configurable: true });
    try {
      expect(() => applyTheme('dark')).not.toThrow();
      expect(document.documentElement).toHaveAttribute('data-theme', 'dark');
      expect(getStoredTheme()).toBe('device');
    } finally {
      Object.defineProperty(window, 'localStorage', { value: real, configurable: true });
    }
  });
});
