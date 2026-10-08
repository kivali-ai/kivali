// What you were writing to an agent survives a reload, per agent, for a week.
// Stored as {"v": text, "ts": savedAt} under kivali.draft.<slug>. Storage can be missing or refuse writes
// (private windows, quota); every access is guarded and the composer works without it.

export const DRAFT_TTL_MS = 7 * 24 * 60 * 60 * 1000;

export function draftKey(slug: string): string {
  return 'kivali.draft.' + slug;
}

function storage(): Storage | null {
  try {
    return typeof window !== 'undefined' ? window.localStorage : null;
  } catch {
    return null;
  }
}

/** The saved draft, or '' when there is none, it is older than a week, or storage is unavailable. */
export function loadDraft(slug: string, now: number = Date.now()): string {
  const s = storage();
  if (!s) return '';
  try {
    const raw = s.getItem(draftKey(slug));
    if (!raw) return '';
    const parsed = JSON.parse(raw) as { v?: unknown; ts?: unknown };
    if (typeof parsed.ts === 'number' && now - parsed.ts > DRAFT_TTL_MS) {
      s.removeItem(draftKey(slug));
      return '';
    }
    return typeof parsed.v === 'string' ? parsed.v : '';
  } catch {
    return '';
  }
}

/** Saves the draft; an empty one is removed. */
export function saveDraft(slug: string, value: string, now: number = Date.now()): void {
  const s = storage();
  if (!s) return;
  try {
    if (value.trim()) s.setItem(draftKey(slug), JSON.stringify({ v: value, ts: now }));
    else s.removeItem(draftKey(slug));
  } catch {
    // Quota or disabled storage: the draft just is not kept.
  }
}

export function clearDraft(slug: string): void {
  saveDraft(slug, '');
}
