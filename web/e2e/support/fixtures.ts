// The suite's `test`: every test gets a `kivali` server and a baseURL at its root, where the app is served.
//
// `seed: 'shared'` (the default) points at the demo server the global setup started. A test
// that changes data sets `test.use({ seed: 'demo' })` (or 'empty' / 'setup') and gets a fresh
// server of its own for just that test, so tests never see each other's writes.
import { test as base, expect } from '@playwright/test';
import type { Page } from '@playwright/test';
import { startServer } from './server';
import type { Seed } from './server';

export interface KivaliServer {
  baseURL: string;
  logPath: string;
}

interface Options {
  seed: Seed | 'shared';
  /** FAKE_CLAUDE_SCENARIO for a fresh server's turns (a message can still tag its own). */
  fakeScenario: string;
  /** devseed -now for a fresh server (RFC 3339); unset means the current minute. */
  seedNow: string | undefined;
  /** How long a fresh server's `slow` scenario waits between deltas (ms); unset means 20 seconds. */
  fakePauseMs: number | undefined;
}

export const test = base.extend<Options & { kivali: KivaliServer }>({
  seed: ['shared', { option: true }],
  fakeScenario: ['reply', { option: true }],
  seedNow: [undefined, { option: true }],
  fakePauseMs: [undefined, { option: true }],
  kivali: async ({ seed, fakeScenario, seedNow, fakePauseMs }, use, testInfo) => {
    if (seed === 'shared') {
      const baseURL = process.env.KIVALI_SHARED_URL;
      if (!baseURL) throw new Error('KIVALI_SHARED_URL is unset: the global setup did not run');
      await use({ baseURL, logPath: process.env.KIVALI_SHARED_LOG ?? '' });
      return;
    }
    const srv = await startServer({
      seed,
      scenario: fakeScenario,
      ...(seedNow ? { now: seedNow } : {}),
      ...(fakePauseMs !== undefined ? { pauseMs: fakePauseMs } : {}),
    });
    try {
      await use(srv);
    } finally {
      if (testInfo.status !== testInfo.expectedStatus) {
        await testInfo.attach('server.log', { path: srv.logPath, contentType: 'text/plain' });
      }
      await srv.stop();
    }
  },
  baseURL: async ({ kivali }, use) => {
    await use(`${kivali.baseURL}/`);
  },
});

export { expect };

/** Whether this test runs in a phone-sized project. */
export function isPhone(page: Page): boolean {
  return (page.viewportSize()?.width ?? 1440) < 700;
}
