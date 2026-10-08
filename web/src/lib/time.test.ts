import { describe, expect, it } from 'vitest';
import { relativeTime } from './time';

const now = Date.parse('2026-09-28T12:00:00Z');

describe('relativeTime', () => {
  it('counts minutes, hours and days, then gives a date', () => {
    expect(relativeTime('2026-09-28T11:59:30Z', now)).toBe('now');
    expect(relativeTime('2026-09-28T11:52:00Z', now)).toBe('8m');
    expect(relativeTime('2026-09-28T09:00:00Z', now)).toBe('3h');
    expect(relativeTime('2026-09-27T10:00:00Z', now)).toBe('Yesterday');
    expect(relativeTime('2026-09-26T10:00:00Z', now)).toBe('2d');
    expect(relativeTime('2026-09-01T12:00:00Z', now)).toBe('1 Sept');
    expect(relativeTime('2025-09-01T12:00:00Z', now)).toBe('1 Sept 2025');
  });

  it('is empty for an unreadable time', () => {
    expect(relativeTime('not a time', now)).toBe('');
  });
});
