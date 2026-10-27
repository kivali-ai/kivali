// The downloads card's words (pages/downloads.ts): a download's size so
// far, its share done, and the line under its name. Pure (downloads.test.ts).

import type { Download } from "../types";

const KB = 1024;
const MB = 1024 * KB;
const GB = 1024 * MB;

/** 512 → "512 B", 86016 → "84 KB", 1258291 → "1.2 MB", 3.4 GB → "3.4 GB". */
export function sizeWords(bytes: number): string {
  if (bytes < KB) return `${bytes} B`;
  if (bytes < MB) return `${Math.max(1, Math.round(bytes / KB))} KB`;
  if (bytes < GB) return `${oneDecimal(bytes / MB)} MB`;
  return `${oneDecimal(bytes / GB)} GB`;
}

/** "1.2 of 4.0 MB", "84 of 512 KB": the part in the whole's unit. */
export function ofWords(done: number, total: number): string {
  if (total < KB) return `${done} of ${total} B`;
  if (total < MB) return `${Math.round(done / KB)} of ${Math.max(1, Math.round(total / KB))} KB`;
  if (total < GB) return `${oneDecimal(done / MB)} of ${oneDecimal(total / MB)} MB`;
  return `${oneDecimal(done / GB)} of ${oneDecimal(total / GB)} GB`;
}

function oneDecimal(n: number): string {
  return n >= 100 ? String(Math.round(n)) : (Math.round(n * 10) / 10).toFixed(1);
}

/** The share done, 0–100, or null when the server gave no length. */
export function downloadPercent(d: Download): number | null {
  if (d.total === null || d.total <= 0) return null;
  return Math.max(0, Math.min(100, Math.floor((d.received / d.total) * 100)));
}

/** The line under the file's name. */
export function downloadLine(d: Download): string {
  switch (d.state) {
    case "downloading":
      if (d.total !== null && d.total > 0) return ofWords(d.received, d.total);
      return d.received === 0 ? "Starting" : `${sizeWords(d.received)} so far`;
    case "done":
      return `Saved in ${d.folder}`;
    case "failed":
      return d.error ? `Couldn't download. ${d.error}` : "Couldn't download.";
  }
}
