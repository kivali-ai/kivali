// Times as the design writes them: "now", "8m", "3h", "Yesterday", "2d", then a date ("26 Sept").

const MONTHS = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'June', 'July', 'Aug', 'Sept', 'Oct', 'Nov', 'Dec'];

const MINUTE = 60_000;
const HOUR = 60 * MINUTE;
const DAY = 24 * HOUR;

/** A short date: "26 Sept", with the year when it is not the current one ("26 Sept 2025"). */
export function shortDate(at: Date, now: Date): string {
  const base = at.getDate() + ' ' + MONTHS[at.getMonth()];
  return at.getFullYear() === now.getFullYear() ? base : base + ' ' + at.getFullYear();
}

/** How long ago `iso` was, relative to `now` (ms since the epoch). An unreadable time is an empty string. */
export function relativeTime(iso: string, now: number): string {
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return '';
  const ago = now - t;
  if (ago < MINUTE) return 'now';
  if (ago < HOUR) return Math.floor(ago / MINUTE) + 'm';
  if (ago < DAY) return Math.floor(ago / HOUR) + 'h';
  if (ago < 2 * DAY) return 'Yesterday';
  if (ago < 7 * DAY) return Math.floor(ago / DAY) + 'd';
  return shortDate(new Date(t), new Date(now));
}
