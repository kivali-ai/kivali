import { describe, expect, it } from 'vitest';
import { browserWindowName, browserWindowPath, internalPath, outsideSite, serverOwned, subagentPath } from './links';

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

describe('the task page', () => {
  it('lives at the task’s path', () => {
    expect(subagentPath('engineering-lead', 'bbbb2222')).toBe('/agents/engineering-lead/subagents/bbbb2222');
    expect(subagentPath('a b', 'x/y')).toBe('/agents/a%20b/subagents/x%2Fy');
  });
});

describe('outsideSite', () => {
  it('keeps the site and its subdomains inside, ignoring a port and case', () => {
    expect(outsideSite('vendor.example', 'vendor.example')).toBe(false);
    expect(outsideSite('login.vendor.example', 'vendor.example')).toBe(false);
    expect(outsideSite('Login.Vendor.Example:8443', 'vendor.example')).toBe(false);
  });

  it('flags another host, including one that only ends in the same letters', () => {
    expect(outsideSite('accounts.idp.example', 'vendor.example')).toBe(true);
    expect(outsideSite('evilvendor.example', 'vendor.example')).toBe(true);
    expect(outsideSite('vendor.example.attacker.test', 'vendor.example')).toBe(true);
  });

  it('flags nothing in the open session', () => {
    expect(outsideSite('anything.test', undefined)).toBe(false);
    expect(outsideSite('anything.test', '')).toBe(false);
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
      '/connect/start?flow=9f86d081',
      '/connect/callback?code=c&state=s',
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
