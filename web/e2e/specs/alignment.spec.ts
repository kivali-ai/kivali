// Alignment: a text box and the button that acts on it sit on one line, the button centred on the
// text box (the design system's button is 36px, its text box 38px). Layout, so only a browser can
// tell.
import type { Locator, Page } from '@playwright/test';
import { expect, test } from '../support/fixtures';

/** The text box labelled label, and the button named button in the same form. */
function row(page: Page, label: string, button: string): { input: Locator; button: Locator } {
  const input = page.getByLabel(label, { exact: true });
  const form = page.locator('form').filter({ has: input });
  return { input, button: form.getByRole('button', { name: button, exact: true }) };
}

async function expectLevel(r: { input: Locator; button: Locator }): Promise<void> {
  await expect(r.input).toBeVisible();
  const a = await r.input.boundingBox();
  const b = await r.button.boundingBox();
  if (!a || !b) throw new Error('no box');
  expect(b.y + b.height / 2, 'the button is centred on the text box').toBeCloseTo(a.y + a.height / 2, 0);
  expect(Math.abs(b.height - a.height), 'the button is about the text box’s height').toBeLessThanOrEqual(2);
}

test('Org: the add fields line up with their buttons', async ({ page }) => {
  await page.goto('org/software');
  await expectLevel(row(page, 'Add software', 'Add'));
  await page.goto('org/network');
  await expectLevel(row(page, 'Allow a host', 'Allow'));
});

test('An agent’s software: the add field lines up with its button', async ({ page }) => {
  await page.goto('agents/engineering-lead/about');
  await page.getByRole('group', { name: 'Document' }).getByRole('button', { name: 'Software' }).click();
  await expectLevel(row(page, 'Add software', 'Add'));
});
