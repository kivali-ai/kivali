import { afterEach, describe, expect, it, vi } from 'vitest';
import { ApiError, apiDelete, apiGet, apiPost, apiPostForm, loginUrl, setUnauthenticatedHandler } from './client';

function respond(status: number, body?: unknown): Response {
  return new Response(body === undefined ? null : JSON.stringify(body), { status });
}

function stub(res: Response | (() => Response)) {
  const fn = vi.fn((_url: string, _init?: RequestInit) => Promise.resolve(typeof res === 'function' ? res() : res));
  vi.stubGlobal('fetch', fn);
  return fn;
}

afterEach(() => {
  vi.unstubAllGlobals();
  setUnauthenticatedHandler(null);
});

describe('api client', () => {
  it('sends Accept JSON, same-origin credentials and parses the body', async () => {
    const fetch = stub(respond(200, { ok: 1 }));
    await expect(apiGet<{ ok: number }>('/api/v1/me')).resolves.toEqual({ ok: 1 });
    const init = fetch.mock.calls[0]?.[1];
    expect(fetch.mock.calls[0]?.[0]).toBe('/api/v1/me');
    expect(init?.credentials).toBe('same-origin');
    expect(init?.method).toBe('GET');
    expect((init?.headers as Record<string, string>).Accept).toBe('application/json');
  });

  it('posts JSON with a content type, and a form without one', async () => {
    const fetch = stub(() => respond(200, {}));
    await apiPost('/api/v1/auto-release', { value: '2m' });
    const json = fetch.mock.calls[0]?.[1];
    expect(json?.body).toBe('{"value":"2m"}');
    expect((json?.headers as Record<string, string>)['Content-Type']).toBe('application/json');

    const form = new FormData();
    form.set('path', 'a');
    await apiPostForm('/api/v1/needs/ack', form);
    const multipart = fetch.mock.calls[1]?.[1];
    expect(multipart?.body).toBe(form);
    expect((multipart?.headers as Record<string, string>)['Content-Type']).toBeUndefined();
  });

  it('deletes, and returns nothing for a 204', async () => {
    const fetch = stub(new Response(null, { status: 204 }));
    await expect(apiDelete('/api/v1/agents/x/pending/1')).resolves.toBeUndefined();
    expect(fetch.mock.calls[0]?.[1]?.method).toBe('DELETE');
  });

  it('turns the plan error shape into an ApiError', async () => {
    stub(respond(403, { error: 'That request came from another site.', who: 'Reload Kivali and try again.' }));
    const err = await apiPost('/api/v1/auto-release', { value: 'now' }).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect(err).toMatchObject({ status: 403, message: 'That request came from another site.', who: 'Reload Kivali and try again.' });
  });

  it('falls back to a plain sentence when the error body is not the plan shape', async () => {
    stub(new Response('<html>bad gateway</html>', { status: 502 }));
    const err = (await apiGet('/api/v1/me').catch((e: unknown) => e)) as ApiError;
    expect(err).toBeInstanceOf(ApiError);
    expect(err.status).toBe(502);
    expect(err.message).not.toContain('html');
    expect(err.who).not.toBe('');
  });

  it('reports an unreachable server as status 0', async () => {
    vi.stubGlobal('fetch', vi.fn(() => Promise.reject(new TypeError('Failed to fetch'))));
    const err = (await apiGet('/api/v1/me').catch((e: unknown) => e)) as ApiError;
    expect(err).toBeInstanceOf(ApiError);
    expect(err.status).toBe(0);
  });

  it('on 401 calls the unauthenticated handler and still rejects', async () => {
    const handler = vi.fn();
    setUnauthenticatedHandler(handler);
    stub(respond(401, { error: 'unauthenticated', who: 'Sign in.' }));
    const err = (await apiGet('/api/v1/snapshot').catch((e: unknown) => e)) as ApiError;
    expect(handler).toHaveBeenCalledTimes(1);
    expect(err.status).toBe(401);
  });

  it('skips the redirect when the caller expects a 401', async () => {
    const handler = vi.fn();
    setUnauthenticatedHandler(handler);
    stub(respond(401, { error: 'unauthenticated', who: 'Sign in.' }));
    await apiGet('/api/v1/me', { redirectOn401: false }).catch(() => undefined);
    expect(handler).not.toHaveBeenCalled();
  });

  it('builds the sign-in URL with the current path as next', () => {
    expect(loginUrl('/team?x=1')).toBe('/login?next=%2Fteam%3Fx%3D1');
  });

  it('by default sends the browser to /login?next=<current path>', async () => {
    const assign = vi.fn();
    vi.stubGlobal('location', { pathname: '/agents/engineering-lead', search: '?tab=1', assign });
    stub(respond(401, { error: 'unauthenticated' }));
    await apiGet('/api/v1/snapshot').catch(() => undefined);
    expect(assign).toHaveBeenCalledWith('/login?next=%2Fagents%2Fengineering-lead%3Ftab%3D1');
  });

  it('does not redirect when already on a sign-in page', async () => {
    const assign = vi.fn();
    vi.stubGlobal('location', { pathname: '/login', search: '', assign });
    stub(respond(401, { error: 'unauthenticated' }));
    await apiGet('/api/v1/snapshot').catch(() => undefined);
    expect(assign).not.toHaveBeenCalled();
  });
});
