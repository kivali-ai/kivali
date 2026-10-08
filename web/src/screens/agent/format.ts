// Short labels the chat shows: message times, model badges, file sizes, durations.

const MONTHS = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'June', 'July', 'Aug', 'Sept', 'Oct', 'Nov', 'Dec'];
const DAYS = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'];
const DAY = 24 * 60 * 60 * 1000;

function hm(d: Date): string {
  return String(d.getHours()).padStart(2, '0') + ':' + String(d.getMinutes()).padStart(2, '0');
}

function startOfDay(t: number): number {
  const d = new Date(t);
  d.setHours(0, 0, 0, 0);
  return d.getTime();
}

/** A message time as the canvas writes it: "09:14" today, "Mon 09:14" this week, "26 Sept 09:14" before. Empty for 0. */
export function messageTime(ts: number, now: number): string {
  if (!ts) return '';
  const d = new Date(ts);
  const days = Math.round((startOfDay(now) - startOfDay(ts)) / DAY);
  if (days <= 0) return hm(d);
  if (days < 7) return DAYS[d.getDay()] + ' ' + hm(d);
  const date = d.getDate() + ' ' + MONTHS[d.getMonth()];
  const year = d.getFullYear() === new Date(now).getFullYear() ? '' : ' ' + d.getFullYear();
  return date + year + ' ' + hm(d);
}

/**
 * The mono badge on a model-written reply and on a background task: "opus 5.5 · high". The server sends the
 * friendly name ("Opus 5.5"); the badge lowers it to the mono-badge form the design writes ("opus · high")
 * and keeps the version, so a chat that mixes a pinned model with the current one stays readable (decision 8).
 * Empty when the row names no model.
 */
export function modelBadge(model?: string, effort?: string): string {
  if (!model) return '';
  const name = model.toLowerCase();
  return effort ? name + ' · ' + effort.toLowerCase() : name;
}

/** "84 KB", "1.2 MB"; empty for an unknown size. */
export function fileSize(bytes: number): string {
  if (!bytes || bytes < 0) return '';
  if (bytes < 1024) return bytes + ' B';
  if (bytes < 1024 * 1024) return Math.round(bytes / 1024) + ' KB';
  return (bytes / (1024 * 1024)).toFixed(1) + ' MB';
}

/** "0.3s", "12s", "1m 48s" between two unix-ms stamps; empty when either is missing. */
export function duration(from?: number, to?: number): string {
  if (!from || !to || to < from) return '';
  const ms = to - from;
  if (ms < 10_000) return (ms / 1000).toFixed(1) + 's';
  const s = Math.round(ms / 1000);
  if (s < 60) return s + 's';
  return Math.floor(s / 60) + 'm ' + (s % 60) + 's';
}
