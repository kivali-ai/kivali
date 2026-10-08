// Words and numbers the pages show: sizes, times, versions, device names.
// Pure; every function takes "now" when it needs one (format.test.ts).

const GB = 1024 * 1024 * 1024;
const MB = 1024 * 1024;

/** 4096 → "4 GB", 1536 → "1.5 GB", 512 → "512 MB". */
export function formatMemory(mb: number): string {
  if (mb < 1024) return `${Math.round(mb)} MB`;
  return `${trim(mb / 1024)} GB`;
}

/** Disk sizes: "18 GB", "2.4 GB", "640 MB". */
export function formatBytes(bytes: number): string {
  if (bytes < GB) return `${Math.max(1, Math.round(bytes / MB))} MB`;
  return `${trim(bytes / GB)} GB`;
}

function trim(n: number): string {
  if (n >= 10) return String(Math.round(n));
  const r = Math.round(n * 10) / 10;
  return Number.isInteger(r) ? String(r) : r.toFixed(1);
}

/** "0.17.0" → "0.17"; "v0.16.2" → "0.16.2"; null stays null. */
export function shortVersion(v: string | null | undefined): string | null {
  if (!v) return null;
  const t = v.replace(/^v/, "");
  return t.replace(/^(\d+\.\d+)\.0$/, "$1");
}

/** "0.16.0" with the leading v dropped, for "Kivali 0.16.0". */
export function fullVersion(v: string | null | undefined): string | null {
  return v ? v.replace(/^v/, "") : null;
}

export function capitalize(s: string): string {
  return s ? s[0].toUpperCase() + s.slice(1) : s;
}

const MONTHS = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];
const DAYS = ["Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"];

function startOfDay(d: Date): number {
  return new Date(d.getFullYear(), d.getMonth(), d.getDate()).getTime();
}

function clock(d: Date): string {
  const h = d.getHours() % 12 || 12;
  return `${h}:${String(d.getMinutes()).padStart(2, "0")}`;
}

function partOfDay(d: Date): string {
  const h = d.getHours();
  if (h < 12) return "morning";
  if (h < 17) return "afternoon";
  if (h < 21) return "evening";
  return "night";
}

/** "2 Oct", or "2 Oct 2025" in another year. */
export function shortDate(d: Date, now: Date): string {
  const base = `${d.getDate()} ${MONTHS[d.getMonth()]}`;
  return d.getFullYear() === now.getFullYear() ? base : `${base} ${d.getFullYear()}`;
}

/**
 * When something happened, in the voice of the paused team page's "Paused since 9:14 this
 * morning": "9:14 this morning", "4:05 yesterday afternoon",
 * "11:30 Monday night", "2 Oct". Local time.
 */
export function sinceWhen(iso: string, now: Date): string | null {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return null;
  const days = Math.round((startOfDay(now) - startOfDay(d)) / 86_400_000);
  const part = partOfDay(d);
  if (days <= 0) return `${clock(d)} ${part === "night" ? "tonight" : `this ${part}`}`;
  if (days === 1) return `${clock(d)} yesterday ${part}`;
  if (days < 7) return `${clock(d)} ${DAYS[d.getDay()]} ${part}`;
  return shortDate(d, now);
}

/** "just now", "5 minutes ago", "2 hours ago", "yesterday", "3 days ago", "2 Oct". */
export function ago(iso: string, now: Date): string | null {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return null;
  const s = Math.max(0, (now.getTime() - d.getTime()) / 1000);
  if (s < 60) return "just now";
  const m = Math.floor(s / 60);
  if (m < 60) return m === 1 ? "1 minute ago" : `${m} minutes ago`;
  const h = Math.floor(m / 60);
  if (h < 24) return h === 1 ? "1 hour ago" : `${h} hours ago`;
  const days = Math.round((startOfDay(now) - startOfDay(d)) / 86_400_000);
  if (days <= 1) return "yesterday";
  if (days < 7) return `${days} days ago`;
  return shortDate(d, now);
}

const MODELS: [string[], string][] = [
  [["macbook", "pro"], "MacBook Pro"],
  [["macbook", "air"], "MacBook Air"],
  [["mac", "mini"], "Mac mini"],
  [["mac", "studio"], "Mac Studio"],
  [["mac", "pro"], "Mac Pro"],
  [["macbook"], "MacBook"],
  [["imac"], "iMac"],
  [["mbp"], "MacBook Pro"],
  [["mba"], "MacBook Air"],
  [["pc"], "PC"],
  [["laptop"], "laptop"],
  [["desktop"], "desktop"],
];

/**
 * A computer's hostname in people's words: "dana-imac.local" → owner
 * "Dana", model "iMac". macOS names hosts "Danas-MacBook-Pro" from
 * "Dana’s MacBook Pro", so a capitalised owner ending in s loses it.
 */
export function parseDevice(device: string): { owner: string | null; model: string | null; host: string } {
  const host = device.replace(/^https?:\/\//, "").replace(/[:/].*$/, "").replace(/\.(local|lan|home|internal)$/i, "");
  const parts = host.split(/[-_ ]+/).filter(Boolean);
  const lower = parts.map((p) => p.toLowerCase());
  for (let i = 0; i < lower.length; i++) {
    for (const [seq, name] of MODELS) {
      if (seq.every((w, j) => lower[i + j] === w) && i + seq.length === lower.length) {
        if (i === 0) return { owner: null, model: name, host };
        if (i !== 1) return { owner: null, model: name, host };
        let owner = parts[0];
        if (/[A-Z]/.test(host) && owner.length > 3 && /s$/.test(owner)) owner = owner.slice(0, -1);
        return { owner: capitalize(owner), model: name, host };
      }
    }
  }
  return { owner: null, model: null, host };
}

/** "Dana’s iMac", "the iMac", or the bare hostname. */
export function deviceName(device: string | null): string {
  if (!device) return "another computer";
  const { owner, model, host } = parseDevice(device);
  if (model && owner) return `${owner}’s ${model}`;
  if (model) return `the ${model}`;
  return host;
}

/** The short label in Settings' sidebar: "iMac", or the hostname. */
export function deviceShort(device: string | null): string {
  if (!device) return "elsewhere";
  const { model, host } = parseDevice(device);
  return model ?? host;
}

/** "Dana Parker" → "DP"; "dana@example.com" → "DA"; "dana.parker@x" → "DP". */
export function initials(nameOrEmail: string): string {
  const base = nameOrEmail.includes("@") ? nameOrEmail.split("@")[0].replace(/[._+-]+/g, " ") : nameOrEmail;
  const words = base.split(/\s+/).filter(Boolean);
  if (words.length === 0) return "?";
  if (words.length === 1) return words[0].slice(0, 2).toUpperCase();
  return (words[0][0] + words[1][0]).toUpperCase();
}

/** The settings folder with the home folder shown as ~. */
export function tildePath(p: string): string {
  return p.replace(/^\/Users\/[^/]+(?=\/|$)/, "~").replace(/^\/home\/[^/]+(?=\/|$)/, "~");
}

/** A plain hostname gets https:// (the shell does the same; this shows it). */
export function normalizeAddress(s: string): string {
  const t = s.trim();
  if (!t) return t;
  if (/^[a-z][a-z0-9+.-]*:\/\//i.test(t)) return t;
  return `https://${t}`;
}
