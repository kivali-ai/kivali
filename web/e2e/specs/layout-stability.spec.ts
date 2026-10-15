// Layout stability: moving between pages, tabs and views never moves the content column sideways,
// not even for a frame. The column is centred, so anything that changes the width it is centred in
// shifts it: most often the page's scrollbar appearing when one view is taller than the window and
// disappearing when the next (or its loading skeleton) is shorter. These tests set the window's
// height between the shortest and the tallest view's, so the steps cross it both ways.
//
// They need scrollbars that take up width, as Linux Chromium's do (CI). Where scrollbars overlay the
// page (headless Chromium on macOS) nothing can move, so they skip rather than pass for nothing. A
// ::-webkit-scrollbar style does not stand in: it gives the scrollbar width, but Chromium still
// treats it as an overlay and reserves no gutter for it, so the fix cannot be seen.
import type { Locator, Page } from '@playwright/test';
import { expect, isPhone, test } from '../support/fixtures';

// Playwright starts headless Chromium with --hide-scrollbars, which would hide the very thing these
// tests are about.
test.use({ launchOptions: { ignoreDefaultArgs: ['--hide-scrollbars'] } });

test.beforeEach(async ({ page }) => {
  test.skip(isPhone(page), 'phones use overlay scrollbars and one column');
  await page.goto('');
  const width = await page.evaluate(() => {
    const probe = document.createElement('div');
    probe.style.cssText = 'position:absolute;top:-999px;width:100px;height:100px;overflow:scroll';
    document.body.appendChild(probe);
    const w = probe.offsetWidth - probe.clientWidth;
    probe.remove();
    return w;
  });
  test.skip(width === 0, 'scrollbars overlay the page here; Linux Chromium (CI) has ones that take up width');
});

interface Step {
  name: string;
  act(): Promise<void>;
  /** Visible once the step's view has fully loaded. */
  settled: Locator;
}

/**
 * Runs the step, recording the content column's horizontal centre on every animation frame from
 * just before it until two frames after it has settled. Returns each distinct centre.
 */
async function centres(page: Page, step: Step): Promise<number[]> {
  await page.evaluate(() => {
    const w = window as unknown as { __centres: number[]; __recording: boolean };
    w.__centres = [];
    w.__recording = true;
    const record = () => {
      const col = document.querySelector('.app-column');
      if (col) {
        const r = col.getBoundingClientRect();
        w.__centres.push(Math.round((r.left + r.right) * 5) / 10);
      }
      if (w.__recording) requestAnimationFrame(record);
    };
    record();
  });
  await step.act();
  await expect(step.settled).toBeVisible();
  return page.evaluate(
    () =>
      new Promise<number[]>((resolve) => {
        const w = window as unknown as { __centres: number[]; __recording: boolean };
        requestAnimationFrame(() =>
          requestAnimationFrame(() => {
            w.__recording = false;
            resolve([...new Set(w.__centres)]);
          }),
        );
      }),
  );
}

/**
 * Walks the steps once in a tall window to learn how far down each view's content reaches; sets the
 * window between the shortest and the tallest, then walks them again: the column's centre must
 * never move.
 */
async function expectStill(page: Page, steps: readonly Step[]): Promise<void> {
  const width = 1440;
  await page.setViewportSize({ width, height: 2400 });
  const heights: number[] = [];
  for (const step of steps) {
    await step.act();
    await expect(step.settled).toBeVisible();
    heights.push(await page.evaluate(() => Math.round(document.querySelector('.app-column')?.getBoundingClientRect().bottom ?? 0)));
  }
  const low = Math.min(...heights);
  const high = Math.max(...heights);
  expect(high - low, 'the views are all the same height, so this cannot show a scrollbar coming and going').toBeGreaterThan(40);
  await page.setViewportSize({ width, height: Math.round((low + high) / 2) });

  const seen = new Set<number>();
  for (const step of steps) {
    (await centres(page, step)).forEach((c) => seen.add(c));
    expect([...seen], `the content column moved during "${step.name}"`).toHaveLength(1);
  }
}

test('the About views keep the column still', async ({ page }) => {
  await page.goto('agents/engineering-lead/about');
  const views = page.getByRole('group', { name: 'Document' });
  const open = (name: string, settled: Locator): Step => ({ name, act: () => views.getByRole('button', { name }).click(), settled });
  await expectStill(page, [
    open('Habits', page.getByText(/^How it works/)),
    open('Memory', page.getByText(/^What it remembers/)),
    open('Software', page.getByRole('heading', { name: 'For every agent' })),
    open('Role', page.getByText(/^What it does/)),
    open('Software', page.getByRole('heading', { name: 'For every agent' })),
    open('Habits', page.getByText(/^How it works/)),
  ]);
});

test('an agent’s tabs keep the column still', async ({ page }) => {
  await page.goto('agents/engineering-lead');
  const tabs = page.getByRole('tablist');
  const open = (name: string): Step => ({
    name,
    act: () => tabs.getByRole('tab', { name: new RegExp('^' + name) }).click(),
    settled: tabs.getByRole('tab', { name: new RegExp('^' + name), selected: true }),
  });
  await expectStill(page, [open('About'), open('Background'), open('Past chats'), open('Chat'), open('About')]);
});

test('moving between pages keeps the column centred', async ({ page }) => {
  await page.goto('');
  const nav = page.getByRole('navigation').first();
  const go = (name: string): Step => {
    const link = nav.getByRole('link', { name: new RegExp('^' + name + '\\b') });
    return { name, act: () => link.click(), settled: link.and(page.locator('[aria-current="page"]')) };
  };
  await expectStill(page, [go('Team'), go('Work'), go('Org'), go('Home')]);
});
