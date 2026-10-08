// Accessibility: axe on every top-level screen against the real server. Only serious and
// critical violations fail a test, and the failure message lists each one (rule, impact,
// targets, help URL) so it says what to fix. Each scan waits for the screen's real content.
import AxeBuilder from '@axe-core/playwright';
import type { Page } from '@playwright/test';
import { expect, isPhone, test } from '../support/fixtures';

type AxeResults = Awaited<ReturnType<AxeBuilder['analyze']>>;

function blocking(results: AxeResults) {
  return results.violations.filter((v) => v.impact === 'serious' || v.impact === 'critical');
}

function summarise(violations: AxeResults['violations']): string {
  return violations
    .map((v) => {
      const targets = v.nodes
        .map((n) => {
          const why = n.failureSummary?.split('\n')[1]?.trim();
          return '    ' + n.target.join(' ') + (why ? '\n      ' + why : '');
        })
        .join('\n');
      return `${v.id} (${v.impact}): ${v.help}\n  ${v.helpUrl}\n${targets}`;
    })
    .join('\n\n');
}

// The Auto-release slider's tick labels (ds/AutoRelease, `.kv-autorel-ticks > span`: --ink-faint
// at 10px, 3.14:1 against the sidebar) are a design decision still open. Only color-contrast skips them, and only them: every other
// rule still scans them, and color-contrast still scans the rest of the page. Drop the second
// pass's exclude once the decision is settled.
const CONTRAST_EXEMPT = '.kv-autorel-ticks > span';

async function expectNoSeriousViolations(page: Page) {
  const everythingButContrast = await new AxeBuilder({ page }).disableRules(['color-contrast']).analyze();
  const contrast = await new AxeBuilder({ page }).withRules(['color-contrast']).exclude(CONTRAST_EXEMPT).analyze();
  const found = [...blocking(everythingButContrast), ...blocking(contrast)];
  // Compare rule ids, not the raw results: the message carries the readable detail, and a
  // deep diff of axe's objects would bury it.
  expect(
    found.map((v) => `${v.id} (${v.impact})`),
    'serious/critical axe violations:\n\n' + summarise(found),
  ).toEqual([]);
}

test.describe('demo screens', () => {
  test('Home', async ({ page }) => {
    await page.goto('');
    await expect(page.getByRole('heading', { name: 'Needs you' })).toBeVisible();
    await expect(page.getByText('Approve hire: Docs writer').first()).toBeVisible();
    await expectNoSeriousViolations(page);
  });

  test('Engineering lead chat', async ({ page }) => {
    await page.goto('agents/engineering-lead');
    await expect(page.getByRole('heading', { name: 'Engineering lead' }).first()).toBeVisible();
    await expect(page.getByRole('textbox', { name: 'Message Engineering lead' })).toBeVisible();
    await expectNoSeriousViolations(page);
  });

  test('hire proposal review', async ({ page, kivali }) => {
    const res = await page.request.get(kivali.baseURL + '/api/v1/home');
    expect(res.ok()).toBe(true);
    const home = (await res.json()) as { needs: { title: string; review_path?: string | null }[] };
    const hire = home.needs.find((n) => n.title === 'Approve hire: Docs writer');
    expect(hire?.review_path, 'the hire proposal has a review path').toMatch(/^\/proposals\//);
    await page.goto(hire!.review_path!.replace(/^\//, ''));
    await expect(page.getByRole('heading', { name: /Docs writer/ }).first()).toBeVisible();
    await expectNoSeriousViolations(page);
  });

  test('Work', async ({ page }) => {
    await page.goto('work');
    await expect(page.getByRole('heading', { name: 'Move shift reminders to the new mail provider' })).toBeVisible();
    await expectNoSeriousViolations(page);
  });

  test('assignment assignments/1', async ({ page }) => {
    await page.goto('assignments/1');
    await expect(page.getByRole('heading', { name: 'Move shift reminders to the new mail provider' })).toBeVisible();
    await expect(page.getByRole('heading', { name: 'Log' })).toBeVisible();
    await expectNoSeriousViolations(page);
  });

  test('Graph', async ({ page }) => {
    await page.goto('graph');
    // Owner cards start closed, so the rows inside them are not visible; each card's head
    // (owner name and node count) is.
    await expect(page.getByText(/^\d+ nodes?$/).first()).toBeVisible();
    await expectNoSeriousViolations(page);
  });

  test('Org', async ({ page }) => {
    await page.goto('org');
    if (isPhone(page)) {
      // The phone shows the list of sections; the desktop shows every section at once.
      await expect(page.getByText('How every agent works')).toBeVisible();
    } else {
      await expect(page.getByRole('heading', { name: 'Handbook' })).toBeVisible();
    }
    await expectNoSeriousViolations(page);
  });
});

test.describe('setup', () => {
  test.use({ seed: 'setup' });

  test('Setup', async ({ page }) => {
    await page.goto('setup');
    await expect(page.getByRole('heading', { name: 'Set up your org' })).toBeVisible();
    await expectNoSeriousViolations(page);
  });
});
