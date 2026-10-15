import { act, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { AgentDoc, AgentSoftware } from '../../api/types.gen';
import { goAgentDetail } from '../../state/fixtures/transcript';
import { agentSoftware, emptyHabits, memoryDoc } from './tabsFixtures';
import { SLUG, deferred, reply, renderTabs } from './tabsHarness';

const PATH = '/agents/' + SLUG + '/about';
const DOC = (kind: string) => '/api/v1/agents/' + SLUG + '/docs/' + kind;
const UPDATED = '2026-09-20T17:00:00Z';

const role: AgentDoc = { kind: 'role', content: '# Engineering lead\n\nYou own the product roadmap.\n\n## What you own\n\n- Releases\n', updated_at: UPDATED, stats: { lines: 6, chars: 80 } };
const habits: AgentDoc = { kind: 'habits', content: '- Read overnight results first.\n', updated_at: UPDATED, stats: { lines: 1, chars: 31 } };

const routes = {
  ['GET ' + DOC('role')]: role,
  ['GET ' + DOC('habits')]: habits,
  ['GET ' + DOC('memory')]: memoryDoc,
};

const page = () => within(screen.getByRole('main'));

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('About: reading', () => {
  it('opens on the role, described in one line, rendered as prose', async () => {
    renderTabs(PATH, { routes });
    expect(await page().findByText('What it does · updated 2 days ago')).toBeInTheDocument();
    // Read first on a hairline line, not in a card.
    expect(page().getByText('What it does · updated 2 days ago').closest('.kv-card')).toBeNull();
    expect(page().getByRole('heading', { name: 'What you own' })).toBeInTheDocument();
    expect(page().getByText('Releases')).toBeInTheDocument();
    expect(page().getByRole('button', { name: 'Role' })).toHaveAttribute('aria-pressed', 'true');
    expect(page().getAllByRole('button', { pressed: false }).map((b) => b.textContent)).toEqual(['Habits', 'Memory', 'Software']);
  });

  it('describes the habits and the memory in their own words', async () => {
    const user = userEvent.setup();
    renderTabs(PATH, { routes });
    await page().findByText(/What it does/);
    await user.click(page().getByRole('button', { name: 'Habits' }));
    expect(await page().findByText('How it works · 1 habit · updated 2 days ago')).toBeInTheDocument();
    expect(page().getByText('Read overnight results first.')).toBeInTheDocument();
    await user.click(page().getByRole('button', { name: 'Memory' }));
    expect(await page().findByText('What it remembers · 12 notes · updated 2 days ago')).toBeInTheDocument();
    expect(page().getByRole('heading', { name: 'Facts' })).toBeInTheDocument();
  });

  it('reads the memory without its provenance markers, and edits it with them', async () => {
    const user = userEvent.setup();
    renderTabs(PATH, { routes });
    await page().findByText(/What it does/);
    await user.click(page().getByRole('button', { name: 'Memory' }));
    expect(await page().findByText('Prod runs v0.14')).toBeInTheDocument();
    expect(page().getByRole('tabpanel', { name: 'About' })).not.toHaveTextContent('[[ep:');
    await user.click(page().getByRole('button', { name: 'Edit memory' }));
    expect(page().getByRole('textbox', { name: 'Memory' })).toHaveValue(memoryDoc.content);
  });

  it('writes an old date without "on"', async () => {
    renderTabs(PATH, { routes: { ...routes, ['GET ' + DOC('role')]: { ...role, updated_at: '2026-08-26T09:00:00Z' } } });
    expect(await page().findByText(/^What it does · updated \d+ Aug$/)).toBeInTheDocument();
  });

  it('says so when a document was never written (agent_doc_empty_habits.json)', async () => {
    const user = userEvent.setup();
    renderTabs(PATH, { routes: { ...routes, ['GET ' + DOC('habits')]: emptyHabits } });
    await page().findByText(/What it does/);
    await user.click(page().getByRole('button', { name: 'Habits' }));
    expect(await page().findByText('How it works · not written yet')).toBeInTheDocument();
    expect(page().getByText('No habits have been written yet.')).toBeInTheDocument();
  });

  it('offers no Edit for an archived agent', async () => {
    renderTabs(PATH, { routes, detail: { ...goAgentDetail, archived: true } });
    await page().findByText(/What it does/);
    expect(page().queryByRole('button', { name: /^Edit/ })).toBeNull();
  });
});

describe.each([
  { kind: 'role', label: 'Role', button: 'Role', doc: role, next: '# Engineering lead\n\nYou own the release.', shown: 'You own the release.', line: /^What it does · updated just now/ },
  { kind: 'habits', label: 'Habits', button: 'Habits', doc: habits, next: '- Read overnight results first.\n- Attach the quote.', shown: 'Attach the quote.', line: /^How it works · 2 habits · updated just now/ },
  { kind: 'memory', label: 'Memory', button: 'Memory', doc: memoryDoc as AgentDoc, next: '# Facts\n- Prod runs v0.15', shown: 'Prod runs v0.15', line: /^What it remembers · 1 note/ },
])('About: editing the $kind', ({ kind, label, button, doc, next, shown, line }) => {
  const saved: AgentDoc = { ...doc, content: next, updated_at: '2026-09-22T16:59:50Z', stats: { lines: 2, chars: next.length, ...(kind === 'memory' ? { notes: 1 } : {}) } };

  async function open() {
    const user = userEvent.setup();
    const h = renderTabs(PATH, { routes: { ...routes, ['PUT ' + DOC(kind)]: saved } });
    await page().findByText(/What it does/);
    if (kind !== 'role') await user.click(page().getByRole('button', { name: button }));
    await user.click(await page().findByRole('button', { name: 'Edit ' + label.toLowerCase() }));
    return { user, h };
  }

  it('swaps the page for an editor holding the text, with Save and Cancel', async () => {
    await open();
    const field = page().getByRole('textbox', { name: label });
    expect(field).toHaveValue(doc.content);
    expect(page().getByText(/Applies from Engineering lead’s next turn/)).toBeInTheDocument();
    expect(page().getByRole('button', { name: 'Save' })).toBeInTheDocument();
    expect(page().getByRole('button', { name: 'Cancel' })).toBeInTheDocument();
  });

  it('Cancel puts the reading page back and discards the change', async () => {
    const { user, h } = await open();
    await user.clear(page().getByRole('textbox', { name: label }));
    await user.type(page().getByRole('textbox', { name: label }), 'Something else');
    await user.click(page().getByRole('button', { name: 'Cancel' }));
    expect(page().queryByRole('textbox')).toBeNull();
    expect(page().queryByText('Something else')).toBeNull();
    expect(h.count('PUT', DOC(kind))).toBe(0);
  });

  it('Save sends the text and shows what the server returned', async () => {
    const { user, h } = await open();
    const field = page().getByRole('textbox', { name: label });
    await user.clear(field);
    await user.click(field);
    await user.paste(next);
    await user.click(page().getByRole('button', { name: 'Save' }));
    expect(await page().findByText(line)).toBeInTheDocument();
    expect(await page().findByText(shown)).toBeInTheDocument();
    expect(page().queryByRole('textbox')).toBeNull();
    const put = h.calls.find((c) => c.method === 'PUT' && c.url === DOC(kind));
    expect(put?.json).toEqual({ content: next });
  });
});

describe('About: software', () => {
  const SOFTWARE = '/api/v1/agents/' + SLUG + '/software';
  const software = agentSoftware as AgentSoftware;

  async function open(extra: Record<string, unknown> = {}) {
    const user = userEvent.setup();
    const h = renderTabs(PATH, { routes: { ...routes, ['GET ' + SOFTWARE]: software, ...extra } });
    await page().findByText(/What it does/);
    await user.click(page().getByRole('button', { name: 'Software' }));
    return { user, h };
  }

  it('shows the org list read-only and the agent’s own list editable', async () => {
    await open();
    expect(await page().findByText('For every agent')).toBeInTheDocument();
    expect(page().getByText('jq')).toBeInTheDocument();
    expect(page().queryByRole('button', { name: 'Remove jq' })).toBeNull();
    expect(page().getByText('Only for Engineering lead')).toBeInTheDocument();
    expect(page().getByRole('button', { name: 'Remove ffmpeg' })).toBeInTheDocument();
    // A listed name that is not a Debian package name is marked, and can be removed.
    expect(page().getByText('x/y')).toBeInTheDocument();
    expect(page().getAllByText('Not a Debian package name, so it is not installed')).toHaveLength(1);
    expect(page().getByRole('button', { name: 'Remove x/y' })).toBeInTheDocument();
    expect(page().getByText(/Changing this list restarts Engineering lead to install it/)).toBeInTheDocument();
  });

  it('adds and removes from the agent’s own list only', async () => {
    const saved: AgentSoftware = { org: software.org, own: ['ffmpeg', 'git-lfs', 'poppler-utils'], invalid: [], unchecked: [] };
    const removed: AgentSoftware = { org: software.org, own: ['git-lfs', 'poppler-utils'], invalid: [], unchecked: [] };
    const { user, h } = await open({ ['POST ' + SOFTWARE]: saved, ['DELETE ' + SOFTWARE + '/ffmpeg']: removed });
    await user.type(await page().findByLabelText('Add software'), 'git-lfs');
    await user.click(page().getByRole('button', { name: 'Add' }));
    expect(await page().findByText('git-lfs')).toBeInTheDocument();
    // Only the new name goes up: the server adds it to the list as it stands.
    expect(h.calls.find((c) => c.method === 'POST' && c.url === SOFTWARE)?.json).toEqual({ packages: ['git-lfs'] });
    await user.click(page().getByRole('button', { name: 'Remove ffmpeg' }));
    await waitFor(() => expect(page().queryByRole('button', { name: 'Remove ffmpeg' })).toBeNull());
    expect(h.count('DELETE', SOFTWARE + '/ffmpeg')).toBe(1);
    expect(h.count('PUT', SOFTWARE)).toBe(0);
  });

  it('shows the server’s refusal of a bad name in a banner', async () => {
    const { user } = await open({ ['POST ' + SOFTWARE]: () => reply(400, { error: '"bad name" is not a Debian package name', who: 'you' }) });
    await user.type(await page().findByLabelText('Add software'), 'bad');
    await user.click(page().getByRole('button', { name: 'Add' }));
    expect(await page().findByText('"bad name" is not a Debian package name')).toBeInTheDocument();
  });

  it('says which names were added without checking them', async () => {
    const saved: AgentSoftware = { org: software.org, own: ['ffmpeg', 'zstd'], invalid: [], unchecked: ['zstd'] };
    const { user } = await open({ ['POST ' + SOFTWARE]: saved });
    expect(page().queryByText(/Added without checking/)).toBeNull();
    await user.type(await page().findByLabelText('Add software'), 'zstd');
    await user.click(page().getByRole('button', { name: 'Add' }));
    expect(await page().findByText('Added without checking zstd')).toBeInTheDocument();
  });

  it('has no Software for an archived agent', async () => {
    renderTabs(PATH, { routes, detail: { ...goAgentDetail, archived: true } });
    await page().findByText(/What it does/);
    expect(page().queryByRole('button', { name: 'Software' })).toBeNull();
  });
});

describe('About: validation', () => {
  it('shows the server’s refusal in a banner and stays in the editor', async () => {
    const user = userEvent.setup();
    renderTabs(PATH, { routes: { ...routes, ['PUT ' + DOC('role')]: () => reply(400, { error: "the role can't be empty", who: 'you' }) } });
    await user.click(await page().findByRole('button', { name: 'Edit role' }));
    await user.clear(page().getByRole('textbox', { name: 'Role' }));
    await user.click(page().getByRole('button', { name: 'Save' }));
    const banner = await page().findByRole('alert');
    expect(banner).toHaveTextContent("The role can't be empty");
    expect(banner).toHaveTextContent('Add some text, then save again.');
    expect(page().getByRole('textbox', { name: 'Role' })).toHaveValue('');
    // Nothing was lost: Cancel returns to the document as it was.
    await user.click(page().getByRole('button', { name: 'Cancel' }));
    expect(page().getByText('What it does · updated 2 days ago')).toBeInTheDocument();
  });

  it('holds Save while the save is out, so a second click sends nothing', async () => {
    const user = userEvent.setup();
    const gate = deferred<AgentDoc>();
    const h = renderTabs(PATH, { routes: { ...routes, ['PUT ' + DOC('role')]: () => gate.promise } });
    await user.click(await page().findByRole('button', { name: 'Edit role' }));
    await user.click(page().getByRole('button', { name: 'Save' }));
    const save = page().getByRole('button', { name: 'Save' });
    expect(save).toBeDisabled();
    expect(page().getByRole('button', { name: 'Cancel' })).toBeDisabled();
    await user.click(save);
    expect(h.count('PUT', DOC('role'))).toBe(1);
    await act(async () => gate.resolve(role));
    expect(await page().findByText('What it does · updated 2 days ago')).toBeInTheDocument();
  });

  it('shows a save that failed for another reason with who can fix it', async () => {
    const user = userEvent.setup();
    renderTabs(PATH, { routes: { ...routes, ['PUT ' + DOC('habits')]: () => reply(500, { error: 'the habits could not be saved', who: 'whoever runs this Kivali server' }) } });
    await page().findByText(/What it does/);
    await user.click(page().getByRole('button', { name: 'Habits' }));
    await user.click(await page().findByRole('button', { name: 'Edit habits' }));
    await user.click(page().getByRole('button', { name: 'Save' }));
    const banner = await page().findByRole('alert');
    expect(banner).toHaveTextContent('The habits could not be saved');
    expect(banner).toHaveTextContent('whoever runs this Kivali server');
  });
});
