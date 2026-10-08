// A proposal's review page against a real server: the demo seed's hire of a docs writer,
// reached from Home's Needs you, its documents, and approving it. The proposal's path is
// date-stamped, so it is read from the Home API rather than written down here.
import type { Page } from '@playwright/test';
import { expect, test } from '../support/fixtures';
import type { KivaliServer } from '../support/fixtures';

const HIRE = 'Approve hire: Docs writer';

/** The hire's router path ("proposals/messages/…md"), from GET /api/v1/home. */
async function hirePath(page: Page, kivali: KivaliServer): Promise<string> {
  const res = await page.request.get(kivali.baseURL + '/api/v1/home');
  expect(res.ok()).toBe(true);
  const home = (await res.json()) as { needs: { title: string; review_path?: string }[] };
  const need = home.needs.find((n) => n.title === HIRE);
  expect(need?.review_path).toMatch(/^\/proposals\//);
  return (need?.review_path ?? '').replace(/^\//, '');
}

test('Review on Home opens the hire proposal', async ({ page, kivali }) => {
  const path = await hirePath(page, kivali);
  await page.goto('');
  await page.getByRole('region', { name: 'Needs you' }).getByRole('link', { name: new RegExp(HIRE) }).click();
  await expect(page).toHaveURL(new RegExp('/' + path.replace(/[.*+?^${}()|[\]\\]/g, '\\$&') + '$'));
  await expect(page.getByRole('heading', { level: 1, name: HIRE })).toBeVisible();
});

test('the hire proposal shows who it hires and the documents it starts with', async ({ page, kivali }) => {
  await page.goto(await hirePath(page, kivali));
  await expect(page.getByRole('heading', { level: 1, name: HIRE })).toBeVisible();
  await expect(page.getByText('Needs your approval')).toBeVisible();
  await expect(page.getByText(/^Proposed by Chief of Staff/)).toBeVisible();
  await expect(page.getByText(/writes every help page alone, and #3 has eleven pages still to go/)).toBeVisible();

  const summary = page.getByRole('region', { name: 'Summary' });
  await expect(summary.getByText('Docs writer').first()).toBeVisible();
  await expect(summary.getByText('New agent')).toBeVisible();
  await expect(summary.getByText('Reports to')).toBeVisible();
  await expect(summary.getByText('Support lead')).toBeVisible();
  await expect(summary.getByText('Opus 5.5 · high')).toBeVisible();

  const role = page.getByRole('region', { name: 'Role', exact: true });
  await expect(role.getByRole('heading', { name: 'Role' })).toBeVisible();
  await expect(role.getByText('role.md · 5 lines')).toBeVisible();
  await expect(role.getByText(/You write Plainsong Rota's help pages/)).toBeVisible();

  const memory = page.getByRole('region', { name: 'Initial memory' });
  await expect(memory.getByText('memory.md · 3 lines')).toBeVisible();
  await expect(memory.getByText(/The shift-swap page is the model for every other help page\./)).toBeVisible();

  const decision = page.getByRole('region', { name: 'Your decision' });
  await expect(decision.getByRole('textbox', { name: 'Note to Chief of Staff (optional)' })).toBeVisible();
  await expect(decision.getByRole('button', { name: 'Deny' })).toBeVisible();
  await expect(decision.getByRole('button', { name: 'Approve hire' })).toBeVisible();
});

test.describe('deciding', () => {
  test.use({ seed: 'demo' });

  test('approving the hire shows the result and takes it off Needs you', async ({ page, kivali }) => {
    await page.goto(await hirePath(page, kivali));
    const decision = page.getByRole('region', { name: 'Your decision' });
    await decision.getByRole('textbox', { name: 'Note to Chief of Staff (optional)' }).fill('Go ahead, and keep me posted.');
    await decision.getByRole('button', { name: 'Approve hire' }).click();

    await expect(page.getByText('You approved this proposal.')).toBeVisible();
    await expect(page.getByText('Approved', { exact: true })).toBeVisible();
    await expect(decision).toHaveCount(0);
    const open = page.getByRole('button', { name: 'Open Docs writer' });
    await expect(open).toBeVisible();

    // The resolution is the server's, not just the page's: a reload shows it too.
    await page.reload();
    await expect(page.getByText('You approved this proposal.')).toBeVisible();
    await expect(page.getByRole('region', { name: 'Your decision' })).toHaveCount(0);

    await page.getByRole('link', { name: 'Back to Home' }).click();
    const needs = page.getByRole('region', { name: 'Needs you' });
    await expect(needs.getByRole('button', { name: /Weekly report: test reminders all delivered/ })).toBeVisible();
    await expect(needs.getByRole('link', { name: new RegExp(HIRE) })).toHaveCount(0);
  });
});
