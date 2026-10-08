// Org: the Usage section (spend tiles, the per-day chart, who spent it, the folded tables and the
// unpriced-model note) and saving the org's name, against the demo seed.
import type { Locator, Page } from '@playwright/test';
import { expect, isPhone, test } from '../support/fixtures';

/** The Usage section: a region of the one-page settings on desktop, its own page on phone. */
async function usage(page: Page): Promise<Locator> {
  await page.goto('org/usage');
  if (isPhone(page)) {
    await expect(page.getByRole('banner').getByRole('heading', { name: 'Usage' })).toBeVisible();
    return page.getByRole('main');
  }
  return page.getByRole('main').getByRole('region', { name: 'Usage' });
}

/** A folded card in the section, named by its heading. */
function card(scope: Locator, page: Page, name: string): Locator {
  return scope.getByRole('group').filter({ has: page.getByRole('heading', { level: 3, name, exact: true }) });
}

const MONEY = String.raw`\$\d+\.\d{2}`;

test.describe('Usage', () => {
  test('tiles show spend for today, 7 days and 30 days', async ({ page }) => {
    const u = await usage(page);
    for (const label of ['Today', '7 days', '30 days']) {
      await expect(u).toContainText(new RegExp(`${label}\\s*${MONEY}`));
    }
    // The calls and cache captions are desktop only.
    if (isPhone(page)) await expect(u.getByText(/calls · \d+% cache hit/)).toHaveCount(0);
    else await expect(u.getByText(/^\d[\d,]* calls · \d+% cache hit$/)).toHaveCount(3);
  });

  test('the per-day chart and who spent it: one ranked bar per agent', async ({ page }) => {
    const u = await usage(page);
    await expect(u.getByText('Spend per day')).toBeVisible();
    await expect(u.getByRole('img', { name: 'Spend per day, last 30 days' })).toBeVisible();
    await expect(u.getByRole('img', { name: 'Spend per day, last 30 days' })).toContainText('Today');

    await expect(u.getByText('Who spent it')).toBeVisible();
    const bars = u.getByRole('progressbar');
    await expect(bars).toHaveCount(5);
    // The seed's heaviest spender leads.
    await expect(u).toContainText(/Who spent it\s*last 30 days\s*Engineering lead/);
    for (const name of ['Engineering lead', 'Chief of Staff', 'Support lead', 'Test runner', 'Bookkeeper']) {
      // The folded By agent table holds the same names out of sight.
      await expect(u.getByText(name, { exact: true }).filter({ visible: true })).toHaveCount(1);
    }
    // Five agents fill the five bars; the rest total appears only past five (RankedBars.test.tsx).
    await expect(u.getByText(/^Everyone else/)).toHaveCount(0);
  });

  test('By agent lists every agent, and Calls and tokens has the three windows', async ({ page }) => {
    const u = await usage(page);
    const byAgent = card(u, page, 'By agent');
    await expect(byAgent).toContainText('All 5 · ranked by 30-day spend');
    await expect(byAgent).toHaveJSProperty('open', false);
    await byAgent.getByRole('heading', { name: 'By agent' }).click();
    await expect(byAgent).toHaveJSProperty('open', true);
    const table = byAgent.getByRole('table');
    await expect(table.getByRole('columnheader')).toHaveText(['Agent', '7 days', '30 days', 'Share']);
    // One row per agent under the header row.
    await expect(table.getByRole('row')).toHaveCount(6);
    await expect(table.getByRole('row', { name: /^Bookkeeper / })).toBeVisible();
    await expect(table.getByRole('row', { name: /^Engineering lead / })).toContainText(new RegExp(MONEY));

    const windows = card(u, page, 'Calls and tokens');
    await windows.getByRole('heading', { name: 'Calls and tokens' }).click();
    await expect(windows).toHaveJSProperty('open', true);
    const wt = windows.getByRole('table');
    await expect(wt.getByRole('columnheader')).toHaveText(['Period', 'Calls', 'Tokens in', 'Tokens out', 'Cache hit', 'Spend']);
    for (const period of ['Last 24 hours', 'Last 7 days', 'Last 30 days']) {
      await expect(wt.getByRole('row', { name: new RegExp('^' + period) })).toBeVisible();
    }
  });

  test('the unpriced model is called out: its tokens are left out of spend', async ({ page }) => {
    const u = await usage(page);
    await expect(u.getByText(/^[\d,]+ tokens this month ran on a model with no list price, so spend leaves them out\.$/)).toBeVisible();
  });
});

test.describe('Organization', () => {
  test.use({ seed: 'demo' });

  test('saving a new name sticks across a reload and shows in the frame', async ({ page, kivali }) => {
    await page.goto('org/organization');
    const main = page.getByRole('main');
    const name = main.getByRole('textbox', { name: 'Name' });
    const save = main.getByRole('button', { name: 'Save' });
    await expect(name).toHaveValue('Plainsong');
    await expect(save).toBeDisabled();

    await name.fill('Fernbrook');
    await expect(save).toBeEnabled();
    const put = page.waitForResponse((r) => r.request().method() === 'PUT' && new URL(r.url()).pathname === '/api/v1/org');
    await save.click();
    expect((await put).ok()).toBe(true);
    // Saved: the field matches the stored name again, so there is nothing to save.
    await expect(save).toBeDisabled();
    // The desktop sidebar follows at once; the phone section page has a back button, not the org mark.
    if (!isPhone(page)) await expect(frameMark(page, 'Fernbrook')).toBeVisible();

    const res = await page.request.get(kivali.baseURL + '/api/v1/org');
    expect(res.ok()).toBe(true);
    expect((await res.json()) as { name: string }).toMatchObject({ name: 'Fernbrook' });

    await page.reload();
    await expect(main.getByRole('textbox', { name: 'Name' })).toHaveValue('Fernbrook');
    if (isPhone(page)) {
      // The org mark sits in the top bar of the tab pages.
      await page.goto('work');
    } else {
      await expect(page.getByRole('complementary', { name: 'Sidebar' }).getByText('Fernbrook', { exact: true })).toBeVisible();
      // The one-page settings head carries the name too.
      await expect(main.getByRole('heading', { level: 1, name: 'Fernbrook' })).toBeVisible();
    }
    await expect(frameMark(page, 'Fernbrook')).toBeVisible();
    await expect(frameMark(page, 'Plainsong')).toHaveCount(0);
  });

  test('a set logo leads with Replace and Remove and no drop zone; removing it brings the drop zone back', async ({ page, kivali }) => {
    void kivali;
    await page.goto('org/organization');
    const main = page.getByRole('main');
    const logo = main.getByRole('img', { name: 'Plainsong logo' });
    await expect(logo).toBeVisible();
    expect((await logo.boundingBox())?.width).toBeGreaterThanOrEqual(56);
    await expect(main.getByRole('button', { name: 'Replace', exact: true })).toBeVisible();
    await expect(main.getByRole('button', { name: 'Remove', exact: true })).toBeVisible();
    await expect(main.getByText('Drop a square logo')).toHaveCount(0);

    const del = page.waitForResponse((r) => r.request().method() === 'DELETE' && new URL(r.url()).pathname === '/api/v1/org/logo');
    await main.getByRole('button', { name: 'Remove', exact: true }).click();
    expect((await del).ok()).toBe(true);
    await expect(main.getByText('Drop a square logo')).toBeVisible();
    await expect(main.getByRole('button', { name: 'Replace', exact: true })).toHaveCount(0);
    await expect(main.getByRole('img', { name: 'Plainsong logo' })).toHaveCount(0);
  });

  test('a blank or unchanged name cannot be saved', async ({ page }) => {
    await page.goto('org/organization');
    const main = page.getByRole('main');
    const name = main.getByRole('textbox', { name: 'Name' });
    const save = main.getByRole('button', { name: 'Save' });
    await expect(name).toHaveValue('Plainsong');
    await name.fill('   ');
    await expect(save).toBeDisabled();
    await name.fill('Plainsong');
    await expect(save).toBeDisabled();
  });
});

/** The org's mark in the frame: the sidebar head on desktop, the top bar on phone. */
function frameMark(page: Page, org: string): Locator {
  const frame = isPhone(page) ? page.getByRole('banner') : page.getByRole('complementary', { name: 'Sidebar' });
  return frame.getByRole('img', { name: org, exact: true });
}

// The desktop one-page settings: the index on the left follows the section being read. The phone
// layout has no index; there the same tests only check that it is absent.
test.describe('Org index', () => {
  function index(page: Page): Locator {
    return page.getByRole('navigation', { name: 'Org sections' });
  }

  /** Exactly one item is current, and it is the named one. */
  async function expectCurrent(page: Page, name: string) {
    const nav = index(page);
    await expect(nav.locator('[aria-current]')).toHaveCount(1);
    await expect(nav.getByRole('link', { name, exact: true })).toHaveAttribute('aria-current', 'page');
  }

  function sectionHeading(page: Page, name: string): Locator {
    return page.getByRole('main').getByRole('region', { name, exact: true }).getByRole('heading', { name, exact: true }).first();
  }

  /** Puts a section's top edge at the top of the scroller. */
  async function scrollTo(page: Page, key: string) {
    await page.locator('#org-' + key).evaluate((el) => el.scrollIntoView({ block: 'start' }));
  }

  test('at load on /org the current item is Usage', async ({ page }) => {
    await page.goto('org');
    if (isPhone(page)) return void (await expect(index(page)).toHaveCount(0));
    await expectCurrent(page, 'Usage');
  });

  test('scrolling moves the current item, and never the URL', async ({ page }) => {
    await page.goto('org');
    if (isPhone(page)) return void (await expect(index(page)).toHaveCount(0));
    const url = page.url();
    await expectCurrent(page, 'Usage');

    await scrollTo(page, 'skills');
    await expect(sectionHeading(page, 'Skills')).toBeInViewport();
    await expectCurrent(page, 'Skills');
    expect(page.url()).toBe(url);

    await scrollTo(page, 'files');
    await expectCurrent(page, 'Project files');

    await page.mouse.move(700, 450);
    await page.mouse.wheel(0, -100000);
    await expectCurrent(page, 'Usage');
    expect(page.url()).toBe(url);
  });

  test('the very bottom of the page marks About current', async ({ page }) => {
    await page.goto('org');
    if (isPhone(page)) return void (await expect(index(page)).toHaveCount(0));
    await page.mouse.move(700, 450);
    await page.mouse.wheel(0, 100000);
    await expectCurrent(page, 'About');
    await expect(sectionHeading(page, 'About')).toBeInViewport();
  });

  test('clicking Skills in the index goes to /org/skills, scrolls there, and Skills is the one current item', async ({ page }) => {
    await page.goto('org');
    if (isPhone(page)) return void (await expect(index(page)).toHaveCount(0));
    await index(page).getByRole('link', { name: 'Skills', exact: true }).click();
    await expect(page).toHaveURL(/\/org\/skills$/);
    await expect(sectionHeading(page, 'Skills')).toBeInViewport();
    await expectCurrent(page, 'Skills');
    // Settled: still Skills after the smooth scroll has ended.
    await expect(sectionHeading(page, 'Skills')).toBeInViewport();
    await expectCurrent(page, 'Skills');
  });

  test('loading /org/network directly shows Network current with its heading in view', async ({ page }) => {
    await page.goto('org/network');
    if (isPhone(page)) return void (await expect(index(page)).toHaveCount(0));
    await expect(sectionHeading(page, 'Network')).toBeInViewport();
    await expectCurrent(page, 'Network');
    await expect(page).toHaveURL(/\/org\/network$/);
  });
});
