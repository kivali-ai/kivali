// The setup wizard on a fresh, empty DATA_DIR: its four steps end in
// a real Chief of Staff hire. The seed call and the Chief of Staff's first turn both run
// through cmd/fake-claude, whose reply becomes the Chief of Staff's memory and first answer.
import type { Page, Response } from '@playwright/test';
import { expect, test } from '../support/fixtures';

test.use({ seed: 'setup' });

const PLAN = [
  '# Plainsong business plan',
  '',
  'We run Plainsong Rota: volunteer shift scheduling and email reminders for food banks and shelters.',
  'Riverside Food Bank moves to our new mail provider this quarter.',
  '',
].join('\n');

interface Progress {
  state: string;
  stages: { label: string; state: string }[];
}

/** Resolves with the first /setup/progress answer that says the hire is done. */
function hireDone(page: Page): Promise<Progress> {
  return page
    .waitForResponse(async (r: Response) => {
      if (!r.url().endsWith('/api/v1/setup/progress') || !r.ok()) return false;
      return ((await r.json()) as Progress).state === 'done';
    })
    .then(async (r) => (await r.json()) as Progress);
}

test('a fresh install sends Home to the wizard', async ({ page }) => {
  await page.goto('');
  await expect(page).toHaveURL(/\/setup$/);
  await expect(page.getByRole('heading', { level: 1, name: 'Set up your org' })).toBeVisible();
});

test('the wizard names the org, takes a project file, and hires the Chief of Staff', async ({ page }) => {
  await page.goto('setup');

  // 1. Welcome
  await expect(page.getByRole('heading', { level: 1, name: 'Set up your org' })).toBeVisible();
  await expect(page.getByText('Step 1 of 4')).toBeVisible();
  await page.getByRole('button', { name: 'Start' }).click();

  // 2. Your org
  await expect(page.getByRole('heading', { level: 1, name: 'Your org' })).toBeVisible();
  await page.getByLabel('Org name').fill('Plainsong');
  await expect(page.getByRole('img', { name: 'Plainsong' })).toBeVisible();
  await page.getByRole('button', { name: 'Continue' }).click();

  // 3. Project files: the step's copy now carries the saved name.
  await expect(page.getByRole('heading', { level: 1, name: 'What your Chief of Staff will read' })).toBeVisible();
  await expect(page.getByText(/It reads these to learn how Plainsong works\./)).toBeVisible();
  await page.locator('input[type="file"]').setInputFiles({
    name: 'business-plan.md',
    mimeType: 'text/markdown',
    buffer: Buffer.from(PLAN),
  });
  await expect(page.getByText('business-plan.md')).toBeVisible();
  await expect(page.getByRole('button', { name: 'Remove business-plan.md' })).toBeVisible();
  await expect(page.getByRole('checkbox', { name: /Write the handbook from these files\?/ })).toBeChecked();
  await page.getByRole('button', { name: 'Continue' }).click();

  // 4. Chief of Staff: fake-claude on PATH is the connected model.
  await expect(page.getByRole('heading', { level: 1, name: 'Hire your Chief of Staff' })).toBeVisible();
  await expect(page.getByText(/once it has read your files it proposes a better handbook/)).toBeVisible();
  await expect(page.getByText('Step 4 of 4')).toBeVisible();
  const done = hireDone(page);
  await page.getByRole('button', { name: 'Hire Chief of Staff' }).click();

  // The hiring panel follows /setup/progress until the seed lands; every stage it listed
  // finished, including the first message that asks for a handbook from the file.
  const progress = await done;
  expect(progress.stages.map((s) => s.label)).toEqual([
    'Reading your files',
    'Writing its first briefing',
    'Hiring',
    'Asking it to draft the handbook',
  ]);
  expect(progress.stages.every((s) => s.state === 'done'), JSON.stringify(progress.stages)).toBe(true);

  // Done
  await expect(page.getByRole('heading', { level: 1, name: 'Your Chief of Staff is hired' })).toBeVisible();
  await expect(page.getByText(/It read your file and is drafting a handbook\./)).toBeVisible();
  await expect(page.getByRole('link', { name: 'Open its chat' })).toHaveAttribute('href', '/agents/chief-of-staff');
  await page.getByRole('button', { name: 'Go to Home' }).click();

  await expect(page).toHaveURL(/^http:\/\/[^/]+\/$/);
  await expect(page.getByRole('heading', { name: 'Needs you' })).toBeVisible();

  // The hire woke the Chief of Staff with its first message, and that turn ran through the
  // fake in its agent runtime: its chat holds the reply.
  await page.goto('agents/chief-of-staff');
  const chat = page.getByRole('list', { name: 'Chat with Chief of Staff' });
  await expect(chat.getByText(/All 500 test reminders went out/)).toBeVisible();
});
