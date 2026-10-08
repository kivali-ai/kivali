// Graph: search, the Flagged and Problems filters, owner sections folded with "Show all N", and a
// node's detail (inline on desktop, a dialog on phone), against the demo seed's seven nodes.
import type { Locator, Page } from '@playwright/test';
import { expect, isPhone, test } from '../support/fixtures';

/** An owner's folded section (a <details> card), named by its heading. */
function owner(page: Page, name: string): Locator {
  return page
    .getByRole('main')
    .getByRole('group')
    .filter({ has: page.getByRole('heading', { level: 3, name, exact: true }) });
}

/** A node's row: its button is named by id, title, owner and short fact. */
function node(page: Page, title: string): Locator {
  return page.getByRole('main').getByRole('button', { name: new RegExp(title) });
}

const ALL_TITLES = [
  'The Riverside Food Bank switch',
  'Which help pages to write',
  'One-day test reminder procedure',
  'Week of test reminders plan',
  'Mail provider switch design',
  'Reminders arrive within 60 s',
  'New help pages brief',
];

/** Opens every owner section so each node row is on the page. */
async function openAll(page: Page) {
  for (const name of ['Test runner', 'Engineering lead', 'Support lead']) {
    const section = owner(page, name);
    await section.getByRole('heading', { level: 3 }).click();
    await expect(section).toHaveJSProperty('open', true);
  }
}

/** Opens a node and returns where its fields show: inline under the row on desktop, a dialog on phone. */
async function openNode(page: Page, section: string, title: string, id: string): Promise<Locator> {
  await owner(page, section).getByRole('heading', { level: 3 }).click();
  await node(page, title).click();
  if (isPhone(page)) {
    const dialog = page.getByRole('dialog', { name: id });
    await expect(dialog).toBeVisible();
    await expect(dialog.getByText(title, { exact: true }).first()).toBeVisible();
    return dialog;
  }
  await expect(node(page, title)).toHaveAttribute('aria-expanded', 'true');
  return owner(page, section);
}

/** The definition that follows a term in a field list. */
function field(scope: Locator, term: string): Locator {
  return scope.getByRole('term').filter({ hasText: new RegExp(`^${term}$`) }).locator('xpath=following-sibling::*[1]');
}

test('readouts and owner sections: the first owner open, the rest folded with counts', async ({ page }) => {
  await page.goto('graph');
  const main = page.getByRole('main');
  await expect(main.getByText('7 nodes', { exact: true })).toBeVisible();
  await expect(main.getByRole('button', { name: 'Flagged · 1' })).toBeVisible();
  await expect(main.getByRole('button', { name: 'Problems · 1' })).toBeVisible();

  await expect(owner(page, 'You')).toHaveJSProperty('open', true);
  await expect(owner(page, 'You')).toContainText('2 nodes');
  await expect(node(page, 'The Riverside Food Bank switch')).toBeVisible();
  await expect(node(page, 'Which help pages to write')).toBeVisible();
  for (const [name, count] of [
    ['Test runner', '2 nodes'],
    ['Engineering lead', '2 nodes'],
    ['Support lead', '1 node'],
  ] as const) {
    await expect(owner(page, name)).toHaveJSProperty('open', false);
    await expect(owner(page, name)).toContainText(count);
  }
  await expect(node(page, 'Mail provider switch design')).toBeHidden();

  await openAll(page);
  for (const t of ALL_TITLES) await expect(node(page, t)).toBeVisible();
  await expect(node(page, 'Mail provider switch design')).toHaveAccessibleName(/^engineering-lead\/mail-switch /);
  await expect(node(page, 'Reminders arrive within 60 s')).toHaveAccessibleName(/^engineering-lead\/reminder-delay /);
});

test('search finds a node by title and puts the query in the URL', async ({ page }) => {
  await page.goto('graph');
  await page.getByRole('searchbox', { name: 'Find a node' }).fill('arrive within');
  await expect(page).toHaveURL(/[?&]q=arrive(\+|%20)within/);
  // A search opens every section it matched in.
  await expect(owner(page, 'Engineering lead')).toHaveJSProperty('open', true);
  await expect(node(page, 'Reminders arrive within 60 s')).toBeVisible();
  await expect(page.getByRole('main').getByRole('button', { name: /^(ceo|test-runner|support-lead)\// })).toHaveCount(0);
  await expect(node(page, 'Mail provider switch design')).toHaveCount(0);

  await page.getByRole('searchbox', { name: 'Find a node' }).fill('test reminder');
  await expect(node(page, 'One-day test reminder procedure')).toBeVisible();
  await expect(node(page, 'Week of test reminders plan')).toBeVisible();
  await expect(node(page, 'Reminders arrive within 60 s')).toHaveCount(0);

  await page.getByRole('searchbox', { name: 'Find a node' }).fill('no such node anywhere');
  await expect(page.getByText('No nodes match those filters.')).toBeVisible();

  // Clearing the box drops the query and folds the list back.
  await page.getByRole('searchbox', { name: 'Find a node' }).fill('');
  await expect(page).toHaveURL(/\/graph$/);
  await expect(owner(page, 'Engineering lead')).toHaveJSProperty('open', false);
});

test('Flagged narrows to the test week plan that rests on a withdrawn procedure', async ({ page }) => {
  await page.goto('graph');
  const flagged = page.getByRole('button', { name: 'Flagged · 1' });
  await flagged.click();
  await expect(flagged).toHaveAttribute('aria-pressed', 'true');
  await expect(page).toHaveURL(/[?&]flagged=1/);
  await expect(node(page, 'Week of test reminders plan')).toBeVisible();
  await expect(node(page, 'Week of test reminders plan').getByRole('img', { name: 'Flagged' })).toBeVisible();
  await expect(page.getByRole('main').getByRole('button', { name: /^(ceo|engineering-lead|support-lead)\// })).toHaveCount(0);
  await expect(node(page, 'One-day test reminder procedure')).toHaveCount(0);

  await flagged.click();
  await expect(flagged).toHaveAttribute('aria-pressed', 'false');
  await expect(page).toHaveURL(/\/graph$/);
  await expect(node(page, 'The Riverside Food Bank switch')).toBeVisible();
});

test('Problems narrows to the brief with an unknown front-matter field', async ({ page }) => {
  await page.goto('graph');
  const problems = page.getByRole('button', { name: 'Problems · 1' });
  await problems.click();
  await expect(problems).toHaveAttribute('aria-pressed', 'true');
  await expect(page).toHaveURL(/[?&]problems=1/);
  await expect(node(page, 'New help pages brief')).toBeVisible();
  await expect(page.getByRole('main').getByRole('button', { name: /^(ceo|engineering-lead|test-runner)\// })).toHaveCount(0);
  if (!isPhone(page)) await expect(node(page, 'New help pages brief')).toContainText('problem');

  // Both filters at once: no node is both.
  await page.getByRole('button', { name: 'Flagged · 1' }).click();
  await expect(page.getByText('No nodes match those filters.')).toBeVisible();
});

test('an owner with more nodes than the first page shows "Show all N"', async ({ page }) => {
  // The demo seed has at most two nodes per owner and the server shows five before "Show all", so
  // the list's first request asks for one per owner. The Show all request goes out untouched.
  await page.route(
    (url) => url.pathname === '/api/v1/graph' && !url.searchParams.has('owner'),
    async (route) => {
      const url = new URL(route.request().url());
      url.searchParams.set('limit', '1');
      await route.continue({ url: url.toString() });
    },
  );
  await page.goto('graph');
  const you = owner(page, 'You');
  await expect(you.getByRole('button', { name: /^ceo\// })).toHaveCount(1);
  const showAll = you.getByRole('button', { name: 'Show all 2' });
  await expect(showAll).toBeVisible();
  await showAll.click();
  await expect(you.getByRole('button', { name: /^ceo\// })).toHaveCount(2);
  await expect(node(page, 'The Riverside Food Bank switch')).toBeVisible();
  await expect(node(page, 'Which help pages to write')).toBeVisible();
  await expect(showAll).toHaveCount(0);
});

test('Week of test reminders plan is flagged, and the detail says why', async ({ page }) => {
  await page.goto('graph');
  const detail = await openNode(page, 'Test runner', 'Week of test reminders plan', 'test-runner/test-week-plan');
  await expect(field(detail, 'Flags')).toHaveText('rests on test-runner/one-day-test, which is withdrawn');
  await expect(field(detail, 'Rests on')).toHaveText('test-runner/one-day-test');
  await expect(field(detail, 'Owner')).toHaveText('Test runner');
  await expect(detail.getByText('Seven days, all 500 addresses, delivered and bounced logged after every send.')).toBeVisible();

  // It closes again.
  if (isPhone(page)) {
    await detail.getByRole('button', { name: 'Close' }).click();
    await expect(detail).toBeHidden();
  } else {
    await node(page, 'Week of test reminders plan').click();
    await expect(node(page, 'Week of test reminders plan')).toHaveAttribute('aria-expanded', 'false');
    await expect(detail.getByRole('term')).toHaveCount(0);
  }
  // The row itself carries the flag.
  await expect(node(page, 'Week of test reminders plan').getByRole('img', { name: 'Flagged' })).toBeVisible();
});

test('New help pages brief shows its problem', async ({ page }) => {
  await page.goto('graph');
  const detail = await openNode(page, 'Support lead', 'New help pages brief', 'support-lead/help-pages-brief');
  await expect(field(detail, 'Problems')).toHaveText('front matter field "audience" is not part of the schema and was ignored');
  await expect(field(detail, 'Path')).toHaveText('help-pages-brief.md');
  await expect(detail.getByText('The new help pages go live in November, one page per task, each with fresh screenshots.')).toBeVisible();
});

// The node's text renders its YAML front matter as a heading. GET /api/v1/graph/nodes/<id>
// (internal/web/api_graph.go) returns body_md as the whole stored file, front matter included, and
// screens/graph/NodeDetail.tsx passes it straight to renderMarkdown: the closing `---` turns the
// front-matter lines into a setext <h2>, e.g. heading "id: test-week-plan depends_on:
// test-runner/one-day-test summary: Week of test reminders plan" under a separator. Un-fixme once
// the server or NodeDetail strips the front matter (the fields already show it).
test('a node\'s text leaves its front matter out', async ({ page }) => {
  await page.goto('graph');
  const detail = await openNode(page, 'Test runner', 'Week of test reminders plan', 'test-runner/test-week-plan');
  await expect(detail.getByText('Seven days, all 500 addresses, delivered and bounced logged after every send.')).toBeVisible();
  await expect(detail.getByRole('heading', { name: /depends_on:/ })).toHaveCount(0);
  await expect(detail.getByText(/id: test-week-plan/)).toHaveCount(0);
});
