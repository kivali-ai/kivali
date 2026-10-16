import { describe, expect, it } from 'vitest';
import { browserWindowName, browserWindowPath, internalPath, serverOwned } from './links';

describe('the browser window', () => {
  it('lives under the task, outside the server-owned paths', () => {
    const path = browserWindowPath('engineering-lead', 'bbbb2222');
    expect(path).toBe('/agents/engineering-lead/subagents/bbbb2222/browser');
    expect(serverOwned(path)).toBe(false);
    expect(browserWindowPath('a b', 'x/y')).toBe('/agents/a%20b/subagents/x%2Fy/browser');
  });

  it('opens under one name per task', () => {
    expect(browserWindowName('bbbb2222')).toBe('kivali-browser-bbbb2222');
  });
});

const ORIGIN = 'http://kivali.test';
const HERE = ORIGIN + '/team';

describe('internalPath', () => {
  it('routes a same-origin app link', () => {
    expect(internalPath('/agents/x?y=1#z', '', ORIGIN, HERE)).toBe('/agents/x?y=1#z');
    expect(internalPath('/', '', ORIGIN, HERE)).toBe('/');
    expect(internalPath(ORIGIN + '/work', '', ORIGIN, HERE)).toBe('/work');
    expect(internalPath('/agents/x/subagents/abc', '', ORIGIN, HERE)).toBe('/agents/x/subagents/abc');
    expect(internalPath('/proposals/messages/2026-09-28/x.md', '', ORIGIN, HERE)).toBe('/proposals/messages/2026-09-28/x.md');
  });

  it('resolves a relative link against the current page', () => {
    expect(internalPath('background', '', ORIGIN, ORIGIN + '/agents/x/')).toBe('/agents/x/background');
  });

  it('leaves what the server answers to the browser', () => {
    for (const href of [
      '/login?next=%2F',
      '/auth/logout',
      '/api/v1/me',
      '/messages/2026-09-28/x.md?dl=1',
      '/attachments/3f2a',
      '/branding/icon-180.png',
      '/assets/index-abc.js',
      '/logos/kivali-icon.svg',
      '/favicon.ico',
      '/healthz',
      '/org/stream',
      '/agents/x/stream',
      '/agents/x/subagents/abc/stream',
    ]) {
      expect(internalPath(href, '', ORIGIN, HERE), href).toBeNull();
    }
    expect(internalPath('https://example.com/team', '', ORIGIN, HERE)).toBeNull();
  });

  it('keeps app paths that only look like server ones', () => {
    for (const path of ['/application', '/apiary', '/agents/stream', '/agents/x/streams', '/messagesx']) {
      expect(serverOwned(path), path).toBe(false);
    }
  });

  it('strips a basename when there is one', () => {
    expect(internalPath('/base/agents/x', '/base', ORIGIN, ORIGIN + '/base/')).toBe('/agents/x');
    expect(internalPath('/elsewhere', '/base', ORIGIN, ORIGIN + '/base/')).toBeNull();
  });
});
