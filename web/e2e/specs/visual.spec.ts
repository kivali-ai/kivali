// Visual baselines: one screenshot per screen state, in every project the config gives this file
// (desktop and phone, light and dark). Runs only with KIVALI_VISUAL=1; see e2e/README.md for how
// the Linux baselines are made in the Playwright image.
//
// Stability: each state gets a fresh server seeded at a fixed instant (the last noon UTC) and a
// browser clock frozen at that instant, so every relative label ("3h ago") is the same on every
// run. Absolute dates still move with the calendar, and spend totals ("today", "7 days") count
// real UTC days on the server, so text that looks like a date, a clock time or an amount is masked.
import type { Locator, Page } from '@playwright/test';
import { expect, test } from '../support/fixtures';
import type { Seed } from '../support/server';

// The most recent 12:00 UTC, so calendar-relative labels ("Yesterday") read the same whatever the
// hour the suite runs at. The server's own clock is up to a day later, which only moves the
// windows it counts itself (spend today, closed this week), and those are masked or wide enough.
const DAY = 86_400_000;
const noon = Math.floor((Date.now() - DAY / 2) / DAY) * DAY + DAY / 2;
const seedNow = new Date(noon).toISOString().replace('.000Z', 'Z');

const DATE_TEXT = /\b(Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec)[a-z]* \d{1,2}\b|\b\d{4}-\d{2}-\d{2}\b|\b\d{1,2}:\d{2}\b|\$[\d,.]+/;

interface State {
  name: string;
  seed: Seed;
  path: (page: Page, api: string) => Promise<string>;
  ready: (page: Page) => Locator;
}

const at = (p: string) => async () => p;

const states: State[] = [
  { name: 'home', seed: 'demo', path: at(''), ready: (p) => p.getByText('Move shift reminders to the new mail provider') },
  { name: 'home-history', seed: 'demo', path: at('?view=history'), ready: (p) => p.getByRole('heading', { name: 'History' }) },
  { name: 'home-empty', seed: 'empty', path: at(''), ready: (p) => p.getByRole('heading', { name: 'Home' }) },
  { name: 'chat', seed: 'demo', path: at('agents/engineering-lead'), ready: (p) => p.getByText('The mail switch is on track.') },
  {
    name: 'proposal-hire',
    seed: 'demo',
    path: async (page, api) => {
      const home = (await (await page.request.get(api + '/api/v1/home')).json()) as { needs: { review_path?: string | null }[] };
      const review = home.needs.find((n) => n.review_path)?.review_path;
      if (!review) throw new Error('no proposal to review in the demo seed');
      return review.replace(/^\//, '');
    },
    ready: (p) => p.getByText('Docs writer'),
  },
  { name: 'work', seed: 'demo', path: at('work'), ready: (p) => p.getByText('Send a week of test reminders') },
  { name: 'assignment', seed: 'demo', path: at('assignments/1'), ready: (p) => p.getByText('Bounce handling checked') },
  { name: 'graph', seed: 'demo', path: at('graph'), ready: (p) => p.getByText('7 nodes') },
  { name: 'org', seed: 'demo', path: at('org'), ready: (p) => p.getByRole('link', { name: /Usage/ }) },
  { name: 'org-usage', seed: 'demo', path: at('org/usage'), ready: (p) => p.getByText('Engineering lead') },
  { name: 'setup', seed: 'setup', path: at('setup'), ready: (p) => p.getByRole('heading') },
];

for (const s of states) {
  test.describe(s.name, () => {
    test.use({ seed: s.seed, seedNow });

    test(`${s.name} looks right`, async ({ page, kivali }) => {
      await page.clock.setFixedTime(new Date(seedNow));
      await page.goto(await s.path(page, kivali.baseURL));
      await expect(s.ready(page).filter({ visible: true }).first()).toBeVisible();
      // Fonts are part of the picture: wait for them rather than for time to pass.
      await page.evaluate(() => document.fonts.ready.then(() => undefined));
      await expect(page).toHaveScreenshot(`${s.name}.png`, {
        fullPage: true,
        mask: [page.getByText(DATE_TEXT), page.locator('time')],
      });
    });
  });
}
