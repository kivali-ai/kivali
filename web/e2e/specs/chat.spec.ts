// The agent chat against a real server and agent runtime, with cmd/fake-claude answering.
// A `[fake:<scenario>]` tag in a message picks the fake's script for that turn.
import type { Page } from '@playwright/test';
import { expect, isPhone, test } from '../support/fixtures';

test.use({ seed: 'demo' });

const REPLY_FINAL = /All 500 test reminders went out/;

function composer(page: Page) {
  return page.getByRole('textbox', { name: 'Message Engineering lead' });
}

async function send(page: Page, text: string) {
  await composer(page).fill(text);
  await page.getByRole('button', { name: 'Send' }).click();
}

function transcript(page: Page) {
  return page.getByRole('list', { name: 'Chat with Engineering lead' });
}

test('a message gets a streamed reply with its tool call collapsed', async ({ page }) => {
  await page.goto('agents/engineering-lead');
  await send(page, 'How is the mail switch going?');
  const chat = transcript(page);
  await expect(chat.getByText('How is the mail switch going?')).toBeVisible();
  await expect(chat.getByText("I'll check the reminders today.")).toBeVisible();
  await expect(chat.getByText(REPLY_FINAL)).toBeVisible();

  // A person's bubble hugs its text: the design system's prose paragraphs carry a bottom margin
  // that chat.css zeroes on the last block, or the text would sit above the bubble's centre.
  const body = chat.locator('.kv-msg--person .kv-msg-body').filter({ hasText: 'How is the mail switch going?' });
  const last = body.locator('.app-chat-prose > :last-child');
  await expect(last).toHaveCSS('margin-bottom', '0px');
  const { height, lineHeight } = await body.evaluate((el) => {
    const text = el.querySelector('.app-chat-prose > :last-child') ?? el;
    return { height: el.getBoundingClientRect().height, lineHeight: parseFloat(getComputedStyle(text).lineHeight) };
  });
  expect(lineHeight).toBeGreaterThan(0);
  expect(height).toBeLessThan(lineHeight * 2.5);

  const tool = chat.locator('details').filter({ hasText: 'file_view' }).last();
  await expect(tool).toBeVisible();
  await expect(tool).toHaveJSProperty('open', false);
  await expect(tool.getByText('500 of 500 sent')).toBeHidden();
  await tool.locator('summary').click();
  await expect(tool.getByText(/500 of 500 sent/)).toBeVisible();
});

// Fails about one run in five. On `done` the chat refetches /chat, and that response can still
// say running:true (the server clears its in-flight mark just after it emits done). The reload
// path in screens/agent/Chat.tsx takes `running` from it but never reopens the stream, so
// nothing turns it off and Stop stays up. Un-fixme once the reload reopens the stream when the
// chat says it is running (the 204 then settles it), or the server clears the mark first.
test('Stop goes away once a turn has finished', async ({ page }) => {
  await page.goto('agents/engineering-lead');
  await send(page, 'How is the mail switch going?');
  await expect(transcript(page).getByText(REPLY_FINAL)).toBeVisible();
  await expect(page.getByRole('button', { name: 'Stop' })).toBeHidden();
});

test('Stop ends a slow turn before it finishes', async ({ page }) => {
  await page.goto('agents/engineering-lead');
  await send(page, 'Walk me through the mail switch [fake:slow]');
  const chat = transcript(page);
  await expect(chat.getByText("I'll", { exact: true })).toBeVisible();
  const stop = page.getByRole('button', { name: 'Stop' });
  await stop.click();
  await expect(stop).toBeHidden();
  // The fake pauses 20s between deltas, so a turn that ignored the interrupt would still be
  // mid-sentence here; the reply never reaches its end.
  await expect(chat.getByText("I'll check the reminders today.")).toHaveCount(0);
  await expect(chat.getByText(REPLY_FINAL)).toHaveCount(0);
  // The chat takes the next message as its own turn.
  await send(page, 'Just the summary then.');
  await expect(chat.getByText(REPLY_FINAL)).toBeVisible();
});

test('a message sent mid-turn waits as pending, then folds into the turn', async ({ page }) => {
  await page.goto('agents/engineering-lead');
  await send(page, 'Check the test reminders [fake:fold]');
  const chat = transcript(page);
  await expect(chat.getByText("I'll", { exact: true })).toBeVisible();

  await send(page, 'Also check the bounces.');
  const pending = chat.locator('.is-pending').filter({ hasText: 'Also check the bounces.' });
  await expect(pending).toBeVisible();
  await expect(chat.getByText('Pending', { exact: true })).toBeVisible();

  // Delivered: the pending bubble becomes an ordinary message and the reply answers it.
  await expect(pending).toHaveCount(0);
  await expect(chat.getByText('Also check the bounces.', { exact: true })).toBeVisible();
  await expect(chat.getByText(/Got your note: “Also check the bounces\.”/)).toBeVisible();
  await expect(chat.getByText(REPLY_FINAL)).toBeVisible();
});

test('a failed model call stops the turn and says so', async ({ page }) => {
  await page.goto('agents/engineering-lead');
  await send(page, 'Re-send the bounced reminders [fake:error]');
  const chat = transcript(page);
  await expect(chat.getByText('Checking the mail log.')).toBeVisible();
  await expect(page.getByText(/overloaded|stopped on an error/i).first()).toBeVisible();
});

test('a subagent dispatch runs through the MCP server and shows its task', async ({ page }) => {
  await page.goto('agents/engineering-lead');
  await send(page, 'Get the test log summarised [fake:subagent]');
  const chat = transcript(page);
  await expect(chat.getByText(/asked a subagent to summarise the test-reminder log/)).toBeVisible();
  await expect(chat.getByText('Summarise the test-reminder log').first()).toBeVisible();
  // The subagent ran as its own fake-claude process in the agent runtime (whose final text is
  // the reply script's), and its result came back to Engineering lead as a delivery that woke it for
  // an ordinary reply: the same sentence in the task that was dispatched, in the result where it
  // landed, and in the reply.
  await expect(chat.getByText(REPLY_FINAL)).toHaveCount(3);
  // The result is a message of its own that says the task finished, closed on the answer until
  // asked for. The seeded chat holds one from an earlier task, which answered something else.
  const fromTask = page.locator('.kv-msg-name', { hasText: /^Background task finished$/ });
  const result = chat.getByRole('listitem').filter({ has: fromTask }).filter({ hasText: REPLY_FINAL });
  await expect(result).toHaveCount(1);
  await expect(result.locator('details')).toHaveJSProperty('open', false);
  await expect(result.getByText(REPLY_FINAL)).toBeHidden();
  await result.locator('summary').click();
  await expect(result.getByText(REPLY_FINAL)).toBeVisible();
  await expect(result.getByRole('link', { name: /Open full session/ })).toBeVisible();
});

test('New chat asks before rotating the chat', async ({ page }) => {
  await page.goto('agents/engineering-lead');
  await page.getByRole('button', { name: 'New chat' }).click();
  const dialog = page.getByRole('dialog', { name: 'Start a new chat with Engineering lead?' });
  await expect(dialog).toBeVisible();
  await expect(dialog).toContainText(
    'Engineering lead first folds this chat into what it remembers and its habits. Then this chat moves to past chats and a fresh one starts. Its assignments and background work carry on.',
  );
  await dialog.getByRole('button', { name: 'Cancel' }).click();
  await expect(dialog).toBeHidden();
});

// A rotation on the real stack: the page that asked for the new chat says so while the agent folds the
// current one, and ends on the fresh chat by itself, without a reload.
test.describe('a new chat', () => {
  // The turn that folds the chat takes about three seconds and then ends by itself: long enough to see
  // the page say a new chat is on its way, and the ordinary end of a rotation rather than a Stop.
  test.use({ fakeScenario: 'slow', fakePauseMs: 1500 });

  test('says a new chat was asked for, then shows the fresh chat without a reload', async ({ page }) => {
    await page.goto('agents/engineering-lead');
    const chat = transcript(page);
    const banner = page.getByText('Engineering lead is starting a new chat.');
    const pastChats = page.getByRole('tab', { name: /Past chats/ });
    await expect(chat.locator(':scope > li').first()).toBeVisible();
    await expect(pastChats).toContainText('1');
    await expect(banner).toHaveCount(0);
    // Anything left on the window is lost to a reload: the fresh chat has to arrive without one.
    await page.evaluate(() => {
      (window as unknown as { kivaliSamePage?: boolean }).kivaliSamePage = true;
    });

    await page.getByRole('button', { name: 'New chat' }).click();
    await page.getByRole('dialog', { name: 'Start a new chat with Engineering lead?' }).getByRole('button', { name: 'Start new chat' }).click();

    // Asked for: the chat area says so, the request is a line in the transcript, and the turn runs under it.
    await expect(banner).toBeVisible();
    await expect(chat.getByText('New chat requested')).toHaveCount(1);
    await expect(chat.getByText("I'll", { exact: true })).toBeVisible();
    await expect(page.getByRole('button', { name: 'Stop' })).toBeVisible();

    // Done: the fresh chat, and nothing of the old one or of the turn that folded it.
    await expect(page.getByText('No messages yet')).toBeVisible({ timeout: 30_000 });
    await expect(banner).toHaveCount(0);
    await expect(chat.locator(':scope > li')).toHaveCount(0);
    await expect(page.getByText('New chat requested')).toHaveCount(0);
    await expect(page.getByText(REPLY_FINAL)).toHaveCount(0);
    await expect(page.getByRole('button', { name: 'Stop' })).toBeHidden();
    await expect(composer(page)).toBeVisible();
    await expect(pastChats).toContainText('2');
    expect(await page.evaluate(() => (window as unknown as { kivaliSamePage?: boolean }).kivaliSamePage)).toBe(true);

    // The fresh chat works: a message gets its reply, and the page stays on it.
    await send(page, 'How is the mail switch going? [fake:reply]');
    await expect(transcript(page).getByText(REPLY_FINAL)).toBeVisible();
    await expect(transcript(page).getByText('New chat requested')).toHaveCount(0);
  });
});

// The agent header and its tabs are pinned to the top of the column: a long chat never
// has to be scrolled back up to reach Background, About or Past chats. Below 960 the header condenses to
// one slim row once the body is scrolled. These only read, so they use the shared demo server.
test.describe('the pinned agent header', () => {
  test.use({ seed: 'shared' });

  // Short enough that the seeded chat scrolls at both widths, so the header has somewhere to go.
  async function openShort(page: Page) {
    const size = page.viewportSize() ?? { width: 1440, height: 900 };
    await page.setViewportSize({ width: size.width, height: 560 });
    await page.goto('agents/engineering-lead');
    await expect(transcript(page).locator(':scope > li').first()).toBeVisible();
  }

  function header(page: Page) {
    return page.locator('header.app-agent-head');
  }

  async function scrollTo(page: Page, where: 'bottom' | 'middle') {
    await page.evaluate((w) => {
      const el = document.scrollingElement ?? document.documentElement;
      const max = el.scrollHeight - el.clientHeight;
      window.scrollTo(0, w === 'bottom' ? max : Math.round(max / 2));
    }, where);
  }

  test('opens at the newest message, above the composer', async ({ page }) => {
    await page.goto('agents/engineering-lead');
    const last = transcript(page).locator(':scope > li').last();
    await expect(last).toBeVisible();
    await expect
      .poll(async () => {
        const box = await last.boundingBox();
        const compose = await page.locator('.app-chat-compose').boundingBox();
        return !!box && !!compose && box.y + box.height <= compose.y + 1 && box.y + box.height > 0;
      })
      .toBe(true);
  });

  test('keeps the tabs in reach at the foot of the chat', async ({ page }) => {
    await openShort(page);
    await scrollTo(page, 'bottom');
    const tabs = page.getByRole('tablist');
    await expect(tabs).toBeInViewport();
    await tabs.getByRole('tab', { name: /Background/ }).click();
    await expect(page).toHaveURL(/\/agents\/engineering-lead\/background$/);
    await expect(page.getByRole('tab', { name: /Background/ })).toHaveAttribute('aria-selected', 'true');
  });

  test('pins the header to the top of the window mid-chat, with the tabs beneath it', async ({ page }) => {
    await openShort(page);
    const room = await page.evaluate(() => (document.scrollingElement ?? document.documentElement).scrollHeight - window.innerHeight);
    expect(room).toBeGreaterThan(200);
    await scrollTo(page, 'middle');
    await expect.poll(async () => (await header(page).boundingBox())?.y).toBe(0);
    await expect
      .poll(async () => {
        const head = await header(page).boundingBox();
        const tabs = await page.getByRole('tablist').boundingBox();
        return head && tabs ? Math.round(tabs.y - (head.y + head.height)) : null;
      })
      .toBe(0);
  });

  test('condenses to one slim row on a phone, and the composer stays in view', async ({ page }) => {
    await openShort(page);
    const phone = (page.viewportSize()?.width ?? 1440) < 960;
    await scrollTo(page, 'middle');
    if (!phone) {
      await expect(header(page)).toHaveClass(/is-pinned/);
      await expect(header(page)).not.toHaveClass(/is-condensed/);
      return;
    }
    await expect(header(page)).toHaveClass(/is-condensed/);
    await expect(header(page).getByText('Engineering lead')).toBeVisible();
    await expect(header(page).locator('.app-agent-role')).toHaveCount(0);
    // One way back on screen and to assistive tech, the slim row's: the phone header's has scrolled away.
    await expect(page.getByRole('button', { name: 'Back to Team' })).toHaveCount(1);
    await expect(header(page).getByRole('button', { name: 'Back to Team' })).toBeInViewport();
    await expect(header(page).getByRole('button', { name: 'New chat' })).toBeInViewport();
    await expect(page.getByRole('heading', { level: 1 })).toHaveCount(1);
    await expect(composer(page)).toBeInViewport();
    // Back at the top it is the full header again, under the phone header and its back link.
    await page.evaluate(() => window.scrollTo(0, 0));
    await expect(header(page)).not.toHaveClass(/is-condensed/);
    await expect(page.getByRole('button', { name: 'Back to Team' })).toHaveCount(1);
    await expect.poll(() => page.evaluate(() => window.scrollY)).toBe(0);
  });

  test('keeps the reading position when the phone header condenses and expands', async ({ page }) => {
    await page.goto('agents/engineering-lead');
    const item = transcript(page).locator(':scope > li').nth(3);
    await expect(item).toBeVisible();
    await page.evaluate(() => window.scrollTo(0, 0));
    const sentinel = await page.evaluate(() => document.querySelector('.app-agent-sentinel')!.getBoundingClientRect().top + window.scrollY);
    const itemTop = async () => Math.round((await item.boundingBox())?.y ?? NaN);
    // Just past the top of the body: the header condenses (on a phone) and the text stays where it was.
    await page.evaluate((y) => window.scrollTo(0, y + 5), sentinel);
    if (isPhone(page)) await expect(header(page)).toHaveClass(/is-condensed/);
    const past = await itemTop();
    // Ten pixels back up: the sentinel is in view again and the header expands; the text moves by the ten only.
    await page.evaluate(() => window.scrollBy(0, -10));
    await expect(header(page)).not.toHaveClass(/is-condensed/);
    await expect.poll(itemTop).toBe(past + 10);
    // And down again.
    await page.evaluate(() => window.scrollBy(0, 10));
    await expect.poll(itemTop).toBe(past);
    if (isPhone(page)) await expect(header(page)).toHaveClass(/is-condensed/);
  });
});
