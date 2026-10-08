/** "482 KB", "1.1 MB": file sizes as the design writes them. */
export function formatBytes(n: number): string {
  if (n < 1024) return n + ' B';
  const kb = n / 1024;
  if (kb < 1024) return Math.round(kb) + ' KB';
  const mb = kb / 1024;
  return (mb < 10 ? mb.toFixed(1) : String(Math.round(mb))) + ' MB';
}

/** Whole seconds as "0:34" or "1:05". */
export function formatElapsed(seconds: number): string {
  const s = Math.max(0, Math.floor(seconds));
  const m = Math.floor(s / 60);
  return m + ':' + String(s % 60).padStart(2, '0');
}
