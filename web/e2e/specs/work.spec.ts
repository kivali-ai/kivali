// Work: the board by goal (Look back / Current / Look forward), an assignment's detail page, and the
// Put on hold dialog, against the demo seed (cmd/devseed/demo.go).
import type { Locator, Page } from '@playwright/test';
import { expect, isPhone, test } from '../support/fixtures';

/** A goal's block on the board, named by its title. */
function goal(page: Page, title: string): Locator {
  return page.getByRole('main').getByRole('region', { name: title, exact: true });
}

/** One of a goal's three columns. */
function column(block: Locator, name: 'Look back' | 'Current' | 'Look forward'): Locator {
  return block.getByRole('region', { name, exact: true });
}

/** One fact cell on an assignment's page, named by its label. */
function fact(page: Page, label: string): Locator {
  return page.getByRole('main').locator('.app-work-fact').filter({ has: page.locator('dt', { hasText: new RegExp(`^${label}$`) }) });
}

/** A board row: the link carries its state word, its title and its #id. */
function row(scope: Locator, title: string, id: number): Locator {
  return scope.getByRole('link', { name: new RegExp(`${title} #${id}\\b`) });
}

test.describe('the board', () => {
  test('readouts count the seeded assignments', async ({ page }) => {
    await page.goto('work');
    const main = page.getByRole('main');
    await expect(main.getByRole('link', { name: '8 open assignments' })).toBeVisible();
    await expect(main.getByRole('link', { name: '3 ready' })).toBeVisible();
    await expect(main.getByRole('link', { name: '1 blocked' })).toBeVisible();
    await expect(main.getByRole('link', { name: '1 on hold' })).toBeVisible();
    await expect(main.getByRole('link', { name: '2 closed this week' })).toBeVisible();
  });

  test('the mail switch goal: closed #2 looks back, blocked #10 is current, #9 and an unclaimed condition look forward', async ({ page }) => {
    await page.goto('work');
    const g = goal(page, 'Move shift reminders to the new mail provider');
    await expect(g.getByRole('heading', { level: 2 })).toHaveText('Move shift reminders to the new mail provider');
    await expect(g).toContainText('1 of 3');

    const back = column(g, 'Look back');
    await expect(back).toContainText('closed this week · 1');
    const two = row(back, 'Set up the new mail provider', 2);
    await expect(two).toBeVisible();
    await expect(two.getByRole('status')).toHaveAccessibleName('Done');
    await expect(two).toHaveAttribute('href', /\/assignments\/2$/);

    const current = column(g, 'Current');
    await expect(current).toContainText('1 blocked');
    const ten = row(current, 'Switch Riverside Food Bank over', 10);
    await expect(ten.getByRole('status')).toHaveAccessibleName('Blocked');
    await expect(ten).toContainText('Waiting on #9');
    await expect(ten).toContainText('Send a week of test reminders');
    await expect(current.getByRole('link')).toHaveCount(1);

    const forward = column(g, 'Look forward');
    await expect(forward).toContainText('ready · 1 unclaimed');
    const nine = row(forward, 'Send a week of test reminders', 9);
    await expect(nine.getByRole('status')).toHaveAccessibleName('Ready');
    await expect(forward.getByText('Bounce handling checked')).toBeVisible();
    await expect(forward.getByText('Unclaimed', { exact: true })).toBeVisible();
    await expect(forward.getByRole('link')).toHaveCount(1);
  });

  test('the help pages goal: closed #4 looks back, held #5 is current, nothing queued', async ({ page }) => {
    await page.goto('work');
    const g = goal(page, 'Publish the new help pages');
    await expect(g).toContainText('1 of 2');

    const four = row(column(g, 'Look back'), 'Write the help page for shift swaps', 4);
    await expect(four.getByRole('status')).toHaveAccessibleName('Done');

    const current = column(g, 'Current');
    await expect(current).toContainText('1 on hold');
    const five = row(current, 'Take screenshots for the help pages', 5);
    await expect(five.getByRole('status')).toHaveAccessibleName('On hold');
    await expect(five).toContainText('On hold by Support lead');

    await expect(column(g, 'Look forward').getByText('Nothing queued')).toBeVisible();
  });

  test('the books goal: #7 and the CEO\'s #8 look forward, nothing else yet', async ({ page }) => {
    await page.goto('work');
    const g = goal(page, 'Close the September books');
    await expect(g).toContainText('0 of 2');
    await expect(column(g, 'Look back').getByText('Nothing closed this week')).toBeVisible();
    await expect(column(g, 'Current').getByText('Nothing in progress')).toBeVisible();

    const forward = column(g, 'Look forward');
    await expect(row(forward, 'Reconcile September card statements', 7).getByRole('status')).toHaveAccessibleName('Ready');
    await expect(row(forward, 'Approve the mail provider contract', 8).getByRole('status')).toHaveAccessibleName('Ready');
    // #8 is grouped under the person, not an agent.
    await expect(forward.getByText('You', { exact: true })).toBeVisible();
    await expect(forward.getByText('Bookkeeper', { exact: true })).toBeVisible();
  });

  test('the blocked readout narrows the board to #10, and Show all brings it back', async ({ page }) => {
    await page.goto('work');
    await page.getByRole('main').getByRole('link', { name: '1 blocked' }).click();
    await expect(page).toHaveURL(/\/work\?state=blocked$/);
    const main = page.getByRole('main');
    await expect(main.getByText('Showing blocked assignments only.')).toBeVisible();
    await expect(row(main, 'Switch Riverside Food Bank over', 10)).toBeVisible();
    await expect(goal(page, 'Publish the new help pages')).toHaveCount(0);
    await expect(row(main, 'Send a week of test reminders', 9)).toHaveCount(0);

    await main.getByRole('link', { name: 'Show all' }).click();
    await expect(page).toHaveURL(/\/work$/);
    await expect(goal(page, 'Publish the new help pages')).toBeVisible();
  });

  test('a row opens its assignment', async ({ page }) => {
    await page.goto('work');
    await row(page.getByRole('main'), 'Send a week of test reminders', 9).click();
    await expect(page).toHaveURL(/\/assignments\/9$/);
    await expect(page.getByRole('heading', { level: 1, name: 'Send a week of test reminders' })).toBeVisible();
  });
});

test.describe('an assignment', () => {
  test('#1 lists its Done-when conditions: met by #2, claimed by #10, unclaimed', async ({ page }) => {
    await page.goto('assignments/1');
    const main = page.getByRole('main');
    await expect(main.getByRole('heading', { level: 1, name: 'Move shift reminders to the new mail provider' })).toBeVisible();
    await expect(main.getByRole('status').first()).toHaveText('Moving');

    const doneWhen = main.getByRole('region', { name: 'Done when' });
    await expect(doneWhen.getByRole('img', { name: '1 of 3 met · 1 in progress · 1 unclaimed' })).toBeVisible();

    const met = doneWhen.getByRole('row', { name: /^Test reminders all delivered/ });
    await expect(met.getByRole('cell').nth(1)).toHaveText('Met');
    await expect(met.getByRole('link', { name: '#2 Set up the new mail provider' })).toHaveAttribute('href', /\/assignments\/2$/);

    const claimed = doneWhen.getByRole('row', { name: /^Riverside Food Bank switched over/ });
    await expect(claimed.getByRole('cell').nth(1)).toHaveText('In progress');
    await expect(claimed.getByRole('link', { name: '#10 Switch Riverside Food Bank over' })).toBeVisible();
    await expect(claimed).toContainText('working');

    const open = doneWhen.getByRole('row', { name: /^Bounce handling checked/ });
    await expect(open.getByRole('cell').nth(1)).toHaveText('Unclaimed');
    await expect(open.getByRole('cell').nth(2)).toHaveText('Nobody yet');

    // Parts: on phone they sit in the folded "More details" card.
    if (isPhone(page)) await main.getByRole('heading', { name: 'More details' }).click();
    // Bare #id refs, as the canvas draws Parts.
    const parts = fact(page, 'Parts').getByRole('definition');
    await expect(parts.getByRole('link')).toHaveCount(3);
    for (const id of [2, 9, 10]) {
      await expect(parts.getByRole('link', { name: '#' + id, exact: true })).toHaveAttribute('href', new RegExp(`/assignments/${id}$`));
    }

    await expect(main.getByRole('region', { name: 'Description' })).toContainText('Riverside Food Bank moves first: about 1,200 reminders a week.');
    await expect(main.getByRole('region', { name: 'Outcome' })).toContainText('Not closed yet. The outcome is written when Engineering lead or you close it.');
  });

  test('#10 is blocked by #9 and says so', async ({ page }) => {
    await page.goto('assignments/10');
    const main = page.getByRole('main');
    await expect(main.getByRole('heading', { level: 1, name: 'Switch Riverside Food Bank over' })).toBeVisible();
    await expect(main.getByRole('status').first()).toHaveText('Blocked');
    await expect(main.getByText('Waiting on #9 Send a week of test reminders, assigned to Test runner.')).toBeVisible();
    // Waits on #9; holds up the goal it counts toward.
    const waitsOn = fact(page, 'Waits on').getByRole('link', { name: '#9', exact: true });
    await expect(waitsOn).toHaveCount(1);
    await expect(main.getByRole('link', { name: '#1 Move shift reminders to the new mail provider' }).first()).toBeVisible();
    await waitsOn.click();
    await expect(page).toHaveURL(/\/assignments\/9$/);
    await expect(main.getByRole('heading', { level: 1, name: 'Send a week of test reminders' })).toBeVisible();
  });

  test('#5 is held with its note in the log', async ({ page }) => {
    await page.goto('assignments/5');
    const main = page.getByRole('main');
    await expect(main.getByRole('status').first()).toHaveText('On hold');
    await expect(main.getByText('On hold by Support lead.')).toBeVisible();
    await expect(main.getByRole('region', { name: 'Log' })).toContainText('Waiting for the new sign-up screen to reach staging.');
  });
});

/**
 * Every fact cell that spills: its content scrolls wider than the cell, or a descendant's box ends past the
 * cell's right edge (1px tolerance). Empty when all fit.
 */
function spills(page: Page): Promise<string[]> {
  return page.locator('.app-work-fact').evaluateAll((cells) =>
    cells.flatMap((cell) => {
      const label = cell.querySelector('dt')?.textContent ?? '?';
      const right = cell.getBoundingClientRect().right;
      const out: string[] = [];
      if (cell.scrollWidth > cell.clientWidth + 1) out.push(`${label}: scrollWidth ${cell.scrollWidth} > clientWidth ${cell.clientWidth}`);
      for (const el of Array.from(cell.querySelectorAll('*'))) {
        const r = el.getBoundingClientRect();
        if (r.width > 0 && r.right > right + 1) out.push(`${label}: <${el.tagName.toLowerCase()} class="${el.className}"> ends at ${r.right}, the cell at ${right}`);
      }
      return out;
    }),
  );
}

/**
 * The type of every piece of text a fact value sets itself: each element that owns a text node, leaving out the
 * AssignmentRef chips and avatar initials, which carry the design system's own type in every place they appear.
 */
function valueTypes(page: Page): Promise<string[]> {
  return page.locator('.app-work-fact-value').evaluateAll((values) =>
    values.flatMap((value) => {
      const owners = new Set<Element>();
      const walk = document.createTreeWalker(value, NodeFilter.SHOW_TEXT);
      for (let n = walk.nextNode(); n; n = walk.nextNode()) {
        const el = n.parentElement;
        if (el && n.textContent?.trim() && !el.closest('.kv-iref, .kv-avatar')) owners.add(el);
      }
      return Array.from(owners, (el) => {
        const s = getComputedStyle(el);
        return `${s.fontSize} ${s.fontFamily}`;
      });
    }),
  );
}

/** Opens the phone's "More details" fold so its four facts are laid out too. */
async function unfold(page: Page) {
  if (!isPhone(page)) return;
  await page.getByRole('main').getByRole('heading', { name: 'More details' }).click();
  await expect(fact(page, 'Dates')).toBeVisible();
}

/** The widths each project checks: the desktop project the three desktop steps, the phone project its own. */
function widths(page: Page): number[] {
  return isPhone(page) ? [390] : [1440, 1200, 960];
}

test.describe("an assignment's facts", () => {
  test('#1 as seeded: every fact sits inside its cell', async ({ page }) => {
    await page.goto('assignments/1');
    await unfold(page);
    await expect(fact(page, 'Parts').getByRole('link')).toHaveCount(3);
    await page.evaluate(() => document.fonts.ready.then(() => undefined));
    for (const width of widths(page)) {
      await page.setViewportSize({ width, height: 900 });
      await expect.poll(() => spills(page), { message: `at ${width} wide` }).toEqual([]);
    }
  });

  for (const n of [1, 4, 12]) {
    test(`#1 with ${n} parts, and as many waits-on, holds-up and counts-toward links: every fact sits inside its cell`, async ({ page }) => {
      // The seed has three parts; the served record is widened to n of each kind of link, long titles and all.
      await page.route(/\/api\/v1\/assignments\/1$/, async (route) => {
        const res = await route.fetch();
        const a = (await res.json()) as {
          parts: { id: number; title: string }[];
          facts: { waits_on: unknown[]; holds_up: unknown[]; counts_toward: unknown[] };
        };
        const seed = a.parts[0] as { id: number; title: string };
        const links = Array.from({ length: n }, (_, i) => ({ ...seed, id: 100 + i, title: `A part with a long title that would never fit a narrow cell, number ${i + 1}` }));
        a.parts = links;
        a.facts.waits_on = links;
        a.facts.holds_up = links;
        a.facts.counts_toward = links.map((l) => ({ id: l.id, title: l.title, condition: `a condition with a long name, number ${l.id}` }));
        await route.fulfill({ response: res, json: a });
      });
      await page.goto('assignments/1');
      await expect(fact(page, 'Waits on').getByRole('link')).toHaveCount(n);
      await unfold(page);
      await expect(fact(page, 'Parts').getByRole('link')).toHaveCount(n);
      await page.evaluate(() => document.fonts.ready.then(() => undefined));
      for (const width of widths(page)) {
        await page.setViewportSize({ width, height: 900 });
        await expect.poll(() => spills(page), { message: `at ${width} wide` }).toEqual([]);
      }
    });
  }

  test('#1: every fact value is set in the one caption type, Dates included', async ({ page }) => {
    await page.goto('assignments/1');
    await unfold(page);
    await expect(fact(page, 'Dates').getByRole('definition')).toContainText(/^Opened /);
    const caption = await page.evaluate(() => getComputedStyle(document.documentElement).getPropertyValue('--type-caption-size').trim());
    // The design's fact-value size: the caption token (the canvas draws 14px, which has no token).
    expect(caption).toBe('13px');
    // One size and one family across all eight values, and the size is the caption's.
    await expect.poll(async () => [...new Set(await valueTypes(page))]).toHaveLength(1);
    await expect.poll(async () => [...new Set((await valueTypes(page)).map((t) => t.split(' ')[0]))]).toEqual([caption]);
    // Dates is among them, at the caption size like the rest.
    const dates = await fact(page, 'Dates').getByRole('definition').locator('span').first().evaluate((el) => getComputedStyle(el).fontSize);
    expect(dates).toBe(caption);
  });
});

test.describe('Put on hold', () => {
  test.use({ seed: 'demo' });

  test('holding #7 posts the note and the assignment shows as held', async ({ page, kivali }) => {
    await page.goto('assignments/7');
    const main = page.getByRole('main');
    await expect(main.getByRole('heading', { level: 1, name: 'Reconcile September card statements' })).toBeVisible();
    await expect(main.getByRole('status').first()).toHaveText('Ready');

    await main.getByRole('button', { name: 'Act' }).click();
    await page.getByRole('menuitem', { name: 'Put on hold' }).click();

    const dialog = page.getByRole('dialog', { name: 'Put #7 on hold?' });
    await expect(dialog).toBeVisible();
    const submit = dialog.getByRole('button', { name: 'Put on hold' });
    await expect(submit).toBeDisabled();
    await dialog.getByRole('textbox', { name: 'Note' }).fill('Wait for the October statement before reconciling.');
    await expect(submit).toBeEnabled();
    await submit.click();
    await expect(dialog).toBeHidden();

    await expect(main.getByRole('status').first()).toHaveText('On hold');
    await expect(main.getByText('On hold by you.')).toBeVisible();
    await expect(main.getByRole('region', { name: 'Log' })).toContainText('Wait for the October statement before reconciling.');

    // The Act menu now offers the release instead.
    await main.getByRole('button', { name: 'Act' }).click();
    await expect(page.getByRole('menuitem', { name: 'Release hold' })).toBeVisible();
    await expect(page.getByRole('menuitem', { name: 'Put on hold' })).toHaveCount(0);
    await page.keyboard.press('Escape');

    const res = await page.request.get(kivali.baseURL + '/api/v1/assignments/7');
    expect(res.ok()).toBe(true);
    expect((await res.json()) as { state: string }).toMatchObject({ state: 'on_hold' });

    // The board shows it held by you.
    await page.goto('work');
    const r = row(column(goal(page, 'Close the September books'), 'Current'), 'Reconcile September card statements', 7);
    await expect(r.getByRole('status')).toHaveAccessibleName('On hold');
    await expect(r).toContainText('On hold by you');
    await expect(page.getByRole('main').getByRole('link', { name: '2 on hold' })).toBeVisible();
  });
});
