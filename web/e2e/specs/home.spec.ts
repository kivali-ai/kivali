// Home against a real server: In flight, Needs you and the Queue on the demo seed, the actions
// that take a row off each list, the auto-release control, and the empty org's empty states.
import type { Page } from '@playwright/test';
import { expect, isPhone, test } from '../support/fixtures';

const GOALS = ['Move shift reminders to the new mail provider', 'Publish the new help pages', 'Close the September books'];
const HIRE = 'Approve hire: Docs writer';
const NOTIFICATION = 'Weekly report: test reminders all delivered';
const ASSIGNMENT = '#8 assigned to you: Approve the mail provider contract';
const CALL = 'Riverside Food Bank call moved to Thursday';
const QUEUED = ['#9 assigned to you: Send a week of test reminders', CALL, 'Mail provider trial extended to Friday'];
const TRIAL = 'Mail provider trial extended to Friday';

function needs(page: Page) {
  return page.getByRole('region', { name: 'Needs you' });
}

function queue(page: Page) {
  return page.getByRole('region', { name: 'Queue' });
}

/** One queued message's row; the row's own class is the only thing that bounds it. */
function queueRow(page: Page, title: string) {
  return queue(page).locator('.kv-qrow').filter({ hasText: title });
}

/** Moves the Queue's auto-release control: the slider on desktop, a Select on phone. */
async function setAutoRelease(page: Page, stop: string) {
  if (isPhone(page)) {
    await queue(page).getByRole('combobox', { name: 'Auto-release' }).selectOption(stop);
  } else {
    await queue(page).getByRole('radiogroup', { name: 'Auto-release' }).getByRole('radio', { name: stop, exact: true }).click();
  }
}

async function expectAutoRelease(page: Page, stop: string) {
  if (isPhone(page)) {
    await expect(queue(page).getByRole('combobox', { name: 'Auto-release' })).toHaveValue(stop);
  } else {
    await expect(queue(page).getByRole('radio', { name: stop, exact: true })).toHaveAttribute('aria-checked', 'true');
  }
}

test('the demo org shows its goals, what needs you and the queue', async ({ page }) => {
  await page.goto('');
  const inFlight = page.getByRole('region', { name: 'In flight' });
  for (const goal of GOALS) await expect(inFlight.getByRole('link', { name: goal })).toBeVisible();

  const n = needs(page);
  await expect(n.getByRole('link', { name: new RegExp(HIRE) })).toBeVisible();
  await expect(n.getByRole('button', { name: new RegExp(NOTIFICATION) })).toBeVisible();
  await expect(n.getByRole('button', { name: new RegExp(ASSIGNMENT.replace('#', '\\#')) })).toBeVisible();

  const q = queue(page);
  for (const title of QUEUED) await expect(queueRow(page, title)).toBeVisible();
  await expect(q.getByText('3 held')).toBeVisible();
  await expect(queueRow(page, TRIAL).getByText('Held', { exact: true })).toBeVisible();
  await expectAutoRelease(page, 'Off');
});

test('the sidebar stays put while the queue filter changes the page height', async ({ page }) => {
  test.skip(isPhone(page), 'the sidebar is desktop only');
  await page.goto('');
  for (const title of QUEUED) await expect(queueRow(page, title)).toBeVisible();
  const sidebar = page.getByRole('complementary', { name: 'Sidebar' });
  const top = async () => (await sidebar.boundingBox())?.y;
  expect(await top(), 'sidebar top before scrolling').toBe(0);

  // At the end of the page, where a shorter list pulls the scroll position back.
  await page.evaluate(() => window.scrollTo(0, document.documentElement.scrollHeight));
  expect(await page.evaluate(() => window.scrollY)).toBeGreaterThan(0);
  expect(await top(), 'sidebar top at the end of the page').toBe(0);
  for (const name of ['Assignments', 'Notices', 'All']) {
    await queue(page).getByRole('button', { name, exact: true }).click();
    await expect(queue(page).getByRole('button', { name, exact: true })).toHaveAttribute('aria-pressed', 'true');
    expect(await top(), 'sidebar top with ' + name).toBe(0);
  }
});

test.describe('changing things', () => {
  test.use({ seed: 'demo' });

  test('acknowledging the notification takes it off Needs you', async ({ page }) => {
    await page.goto('');
    const row = needs(page).getByRole('button', { name: new RegExp(NOTIFICATION) });
    await row.click();
    await needs(page).getByRole('button', { name: 'Acknowledge' }).click();
    await expect(row).toHaveCount(0);
    await page.reload();
    await expect(needs(page).getByRole('link', { name: new RegExp(HIRE) })).toBeVisible();
    await expect(needs(page).getByRole('button', { name: new RegExp(NOTIFICATION) })).toHaveCount(0);
  });

  test('handing back the CEO assignment takes it off Needs you', async ({ page }) => {
    await page.goto('');
    const row = needs(page).getByRole('button', { name: /assigned to you: Approve the mail provider contract/ });
    await row.click();
    const hand = needs(page).getByRole('button', { name: 'Hand back' });
    // An outcome is required.
    await hand.click();
    await expect(needs(page).getByText('Say what was done, or why it is dropped.')).toBeVisible();
    await needs(page).getByRole('textbox', { name: 'Outcome' }).fill('Approved the $1,800 mail provider contract.');
    await hand.click();
    await expect(row).toHaveCount(0);
    await page.reload();
    await expect(needs(page).getByRole('button', { name: new RegExp(NOTIFICATION) })).toBeVisible();
    await expect(needs(page).getByRole('button', { name: /assigned to you: Approve the mail provider contract/ })).toHaveCount(0);
  });

  test('releasing a queued message takes it off the queue', async ({ page }) => {
    await page.goto('');
    await expect(queue(page).getByText('3 held')).toBeVisible();
    await queueRow(page, TRIAL).getByRole('button', { name: 'Release', exact: true }).click();
    await expect(queueRow(page, TRIAL)).toHaveCount(0);
    await expect(queue(page).getByText('2 held')).toBeVisible();
    await page.reload();
    await expect(queueRow(page, CALL)).toBeVisible();
    await expect(queueRow(page, TRIAL)).toHaveCount(0);
  });

  test('an expanded notice: Bounce with note needs a note, and bouncing takes the row off the queue', async ({ page }) => {
    await page.goto('');
    await expect(queue(page).getByText('3 held')).toBeVisible();
    const row = queueRow(page, CALL);
    await row.getByRole('button', { name: CALL }).click();
    const bounce = row.locator('.kv-qrow-body').getByRole('button', { name: 'Bounce with note', exact: true });
    const release = row.locator('.kv-qrow-body').getByRole('button', { name: 'Release', exact: true });
    await expect(bounce).toBeDisabled();
    await expect(release).toBeEnabled();
    await row.locator('.kv-qrow-body').getByRole('textbox', { name: 'Note to the recipient (optional)' }).fill('Hold off until the test week is done.');
    await expect(bounce).toBeEnabled();
    const post = page.waitForResponse((r) => r.request().method() === 'POST' && new URL(r.url()).pathname === '/api/v1/queue/bounce');
    await bounce.click();
    expect((await post).ok()).toBe(true);
    await expect(page.getByRole('dialog')).toHaveCount(0);
    await expect(row).toHaveCount(0);
    await expect(queue(page).getByText('2 held')).toBeVisible();
    await page.reload();
    await expect(queueRow(page, CALL)).toHaveCount(0);
    await expect(queue(page).getByText('2 held')).toBeVisible();
  });

  test('an expanded notice: Release with a note takes the row off the queue', async ({ page }) => {
    await page.goto('');
    await expect(queue(page).getByText('3 held')).toBeVisible();
    const row = queueRow(page, CALL);
    await row.getByRole('button', { name: CALL }).click();
    const item = row.locator('.kv-qrow-body');
    await item.getByRole('textbox', { name: 'Note to the recipient (optional)' }).fill('Agreed. Keep it as is.');
    await item.getByRole('button', { name: 'Release', exact: true }).click();
    await expect(page.getByRole('dialog')).toHaveCount(0);
    await expect(row).toHaveCount(0);
    await expect(queue(page).getByText('2 held')).toBeVisible();
    await page.reload();
    await expect(queueRow(page, CALL)).toHaveCount(0);
  });

  test('an expanded assignment row has no bounce action', async ({ page }) => {
    await page.goto('');
    const row = queueRow(page, QUEUED[0] as string);
    await row.getByRole('button', { name: QUEUED[0] as string }).click();
    const item = row.locator('.kv-qrow-body');
    await expect(item.getByRole('button', { name: 'Release', exact: true })).toBeEnabled();
    await expect(item.getByRole('button', { name: /Bounce/ })).toHaveCount(0);
    await expect(page.getByRole('dialog')).toHaveCount(0);
  });

  test('auto-release follows the control and sticks', async ({ page }) => {
    await page.goto('');
    const q = queue(page);
    await expect(q.getByText('3 held')).toBeVisible();
    // Off: rows can be picked for "Release N selected".
    await expect(q.getByRole('checkbox', { name: 'Select message' })).toHaveCount(3);

    await setAutoRelease(page, '20m');
    await expectAutoRelease(page, '20m');
    await expect(q.getByText('3 passing through')).toBeVisible();
    await expect(q.getByRole('checkbox', { name: 'Select message' })).toHaveCount(0);
    // A new setting governs what is sent from now on: what was queued under Off stays held.
    await expect(queueRow(page, TRIAL).getByText('Held', { exact: true })).toBeVisible();

    await page.reload();
    await expectAutoRelease(page, '20m');
    await expect(q.getByText('3 passing through')).toBeVisible();

    await setAutoRelease(page, 'Off');
    await expect(q.getByText('3 held')).toBeVisible();
    await expect(q.getByRole('checkbox', { name: 'Select message' })).toHaveCount(3);
  });

  test('a queued row counts down under auto-release and reads Held under Off', async ({ page, kivali }) => {
    // Every seeded row was queued under Off, so none carries a deadline and nothing can queue a
    // new message from the page. The Home response is given one row with a deadline two minutes
    // out, and the page's clock is driven by hand, so the countdown is the page's own arithmetic.
    const t0 = Math.floor(Date.now() / 1000) * 1000;
    const now = t0 + 1000;
    await page.clock.install({ time: t0 });
    await page.clock.pauseAt(now);
    // Read once, served as-is to every load: a handler that fetched per request could still be
    // mid-fetch when the test's server stops.
    const res = await page.request.get(kivali.baseURL + '/api/v1/home');
    const body = (await res.json()) as { queue: { title: string; held: boolean; releases_at?: string }[] };
    for (const q of body.queue) {
      if (q.title === TRIAL) {
        q.held = false;
        q.releases_at = new Date(now + 120_000).toISOString();
      }
    }
    await page.route('**/api/v1/home', (route) => route.fulfill({ json: body }));
    await page.goto('');
    const row = queueRow(page, TRIAL);
    await expect(row.getByText('Held', { exact: true })).toBeVisible();

    await setAutoRelease(page, '2m');
    await expect(row.getByText('Releases in 2:00')).toBeVisible();
    await page.clock.runFor(30_000);
    await expect(row.getByText('Releases in 1:30')).toBeVisible();
    await page.clock.runFor(90_000);
    await expect(row.getByText('Releasing')).toBeVisible();
    // The other rows were queued under Off and stay held.
    await expect(queueRow(page, CALL).getByText('Held', { exact: true })).toBeVisible();

    await setAutoRelease(page, 'Off');
    await expect(row.getByText('Held', { exact: true })).toBeVisible();
  });
});

test.describe('an org with nothing going on', () => {
  test.use({ seed: 'empty' });

  test('shows the empty states', async ({ page }) => {
    await page.goto('');
    await expect(page.getByRole('region', { name: 'In flight' }).getByText('No goals in flight. Goals are the top-level assignments your team works toward.')).toBeVisible();
    await expect(needs(page).getByRole('heading', { name: 'Nothing waiting on you' })).toBeVisible();
    await expect(needs(page).getByText('nothing waiting', { exact: true })).toBeVisible();
    await expect(queue(page).getByRole('heading', { name: 'No deliveries queued' })).toBeVisible();
    await expect(queue(page).getByText('empty', { exact: true })).toBeVisible();
    await expect(queue(page).getByRole('button', { name: 'Release all' })).toHaveCount(0);
  });
});
