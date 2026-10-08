// A message the agent sent: the chat's sent line opens in place to the message as a card, and the
// stored markdown file is only reachable as the card's "Raw message" link. On the demo seed the
// Engineering lead published "Mail provider trial extended to Friday" to Test runner.
import { expect, test } from '../support/fixtures';

const LINE = 'Sent “Mail provider trial extended to Friday” to Test runner';

test('the sent line opens to the message, and only Raw message links to the stored file', async ({ page }) => {
  await page.goto('agents/engineering-lead');
  const chat = page.getByRole('list', { name: 'Chat with Engineering lead' });
  const line = chat.getByRole('button', { name: new RegExp(LINE) });
  await expect(line).toBeVisible();
  await expect(line).toHaveAttribute('aria-expanded', 'false');
  await expect(chat.getByText(/The mail provider extended our trial to Friday/)).toHaveCount(0);

  // Nothing in the chat is an "Open" link, and no link but Raw message points at a .md file.
  await expect(chat.getByRole('link', { name: 'Open' })).toHaveCount(0);

  await line.click();
  await expect(line).toHaveAttribute('aria-expanded', 'true');
  await expect(chat.getByRole('heading', { name: 'Mail provider trial extended to Friday' })).toBeVisible();
  await expect(chat.getByText('Sent to Test runner')).toBeVisible();
  await expect(chat.getByText('The mail provider extended our trial to Friday. Send every test reminder from the trial account until then.')).toBeVisible();

  const toFiles = await chat.getByRole('link').evaluateAll((links) =>
    links.filter((a) => /\.md$/.test(a.getAttribute('href') ?? '')).map((a) => a.textContent?.trim() ?? ''),
  );
  expect(toFiles).toEqual(['Raw message']);
  const raw = chat.getByRole('link', { name: 'Raw message' });
  await expect(raw).toHaveAttribute('target', '_blank');
  const href = await raw.getAttribute('href');
  expect(href).toMatch(/^\/messages\/.+\.md$/);
  const res = await page.request.get(href ?? '');
  expect(res.ok()).toBe(true);
  expect(await res.text()).toContain('The mail provider extended our trial to Friday');

  await line.click();
  await expect(chat.getByRole('heading', { name: 'Mail provider trial extended to Friday' })).toHaveCount(0);
});
