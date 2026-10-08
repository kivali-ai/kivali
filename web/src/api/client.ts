import type { ErrorBody } from './types.gen';

/**
 * An error response from /api/v1: what happened (`message`) and who can fix it (`who`).
 * `status` is the HTTP status, or 0 when the server could not be reached at all. `body` is the parsed JSON
 * error body when there was one, for the few endpoints whose refusal carries more than `error` and `who`
 * (a skill downgrade's 409 names both versions).
 */
export class ApiError extends Error {
  readonly status: number;
  readonly who: string;
  readonly body: unknown;

  constructor(status: number, message: string, who: string, body?: unknown) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
    this.who = who;
    this.body = body;
  }
}

/** The app's sign-in screen, coming back to `returnTo` afterwards. */
export function loginUrl(returnTo: string): string {
  return '/login?next=' + encodeURIComponent(returnTo);
}

function isLoginPath(path: string): boolean {
  return path === '/login' || path.startsWith('/login/');
}

function redirectToLogin(): void {
  const here = window.location.pathname + window.location.search;
  // Already on a sign-in page: navigating again would loop.
  if (isLoginPath(window.location.pathname)) return;
  window.location.assign(loginUrl(here));
}

let onUnauthenticated: () => void = redirectToLogin;

/** Replaces what happens on a 401. Tests use it; the default sends the browser to /login?next=. */
export function setUnauthenticatedHandler(handler: (() => void) | null): void {
  onUnauthenticated = handler ?? redirectToLogin;
}

export interface RequestOptions {
  signal?: AbortSignal;
  /** Set false for calls that expect a 401 (the sign-in screen probing for the org). Default true. */
  redirectOn401?: boolean;
}

const FALLBACK_MESSAGE = 'The server sent a response Kivali could not read.';
const FALLBACK_WHO = 'Whoever runs this Kivali server can look into it.';

async function toApiError(res: Response): Promise<ApiError> {
  let parsed: unknown;
  try {
    parsed = await res.json();
  } catch {
    parsed = undefined;
  }
  const body = (parsed !== null && typeof parsed === 'object' ? parsed : {}) as Partial<ErrorBody>;
  const message = typeof body.error === 'string' && body.error ? body.error : FALLBACK_MESSAGE;
  const who = typeof body.who === 'string' && body.who ? body.who : FALLBACK_WHO;
  return new ApiError(res.status, message, who, parsed);
}

/** Sends one request and hands back a successful response; every failure becomes an ApiError. */
async function send(method: string, path: string, body: BodyInit | null, extra: Record<string, string>, opts: RequestOptions): Promise<Response> {
  let res: Response;
  try {
    res = await fetch(path, {
      method,
      credentials: 'same-origin',
      headers: { Accept: 'application/json', ...extra },
      body,
      signal: opts.signal ?? null,
    });
  } catch (err) {
    if (err instanceof DOMException && err.name === 'AbortError') throw err;
    throw new ApiError(0, 'Kivali could not reach the server.', 'Check your connection, then try again. If it keeps happening, whoever runs this Kivali server can look into it.');
  }
  if (res.status === 401 && opts.redirectOn401 !== false) onUnauthenticated();
  if (!res.ok) throw await toApiError(res);
  return res;
}

async function request<T>(method: string, path: string, body: BodyInit | null, extra: Record<string, string>, opts: RequestOptions): Promise<T> {
  const res = await send(method, path, body, extra, opts);
  if (res.status === 204) return undefined as T;
  try {
    return (await res.json()) as T;
  } catch {
    throw new ApiError(res.status, FALLBACK_MESSAGE, FALLBACK_WHO);
  }
}

export function apiGet<T>(path: string, opts: RequestOptions = {}): Promise<T> {
  return request<T>('GET', path, null, {}, opts);
}

export function apiPost<T>(path: string, body?: unknown, opts: RequestOptions = {}): Promise<T> {
  if (body === undefined) return request<T>('POST', path, null, {}, opts);
  return request<T>('POST', path, JSON.stringify(body), { 'Content-Type': 'application/json' }, opts);
}

export function apiPut<T>(path: string, body?: unknown, opts: RequestOptions = {}): Promise<T> {
  if (body === undefined) return request<T>('PUT', path, null, {}, opts);
  return request<T>('PUT', path, JSON.stringify(body), { 'Content-Type': 'application/json' }, opts);
}

export function apiDelete<T = void>(path: string, opts: RequestOptions = {}): Promise<T> {
  return request<T>('DELETE', path, null, {}, opts);
}

/** POST a multipart form (attachments). The browser sets the boundary, so no Content-Type here. */
export function apiPostForm<T>(path: string, form: FormData, opts: RequestOptions = {}): Promise<T> {
  return request<T>('POST', path, form, {}, opts);
}

/** A file the server sent back: its bytes and the name from Content-Disposition, when it gave one. */
export interface Download {
  blob: Blob;
  filename: string | null;
}

function filenameOf(res: Response): string | null {
  const m = /filename="?([^";]+)"?/.exec(res.headers.get('content-disposition') ?? '');
  return m?.[1] ?? null;
}

async function download(method: string, path: string, opts: RequestOptions): Promise<Download> {
  const res = await send(method, path, null, { Accept: '*/*' }, opts);
  return { blob: await res.blob(), filename: filenameOf(res) };
}

/** GET a file rather than JSON (a project file's original). Errors are ApiErrors, as everywhere. */
export function apiGetBlob(path: string, opts: RequestOptions = {}): Promise<Download> {
  return download('GET', path, opts);
}
