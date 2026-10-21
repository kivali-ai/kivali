import { act, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { AgentDoc, AgentNetwork, AgentSoftware } from '../../api/types.gen';
import { goAgentDetail } from '../../state/fixtures/transcript';
import { agentNetwork, agentNetworkExempt, agentSoftware, emptyHabits, memoryDoc } from './tabsFixtures';
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
    expect(page().getAllByRole('button', { pressed: false }).map((b) => b.textContent)).toEqual(['Habits', 'Memory', 'Software', 'Security']);
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
    const saved: AgentSoftware = { org: software.org, own: ['ffmpeg', 'xz-utils', 'zstd'], invalid: [], unchecked: ['xz-utils', 'zstd'] };
    const { user } = await open({ ['POST ' + SOFTWARE]: saved });
    expect(page().queryByText(/couldn’t be reached/)).toBeNull();
    await user.type(await page().findByLabelText('Add software'), 'zstd');
    await user.click(page().getByRole('button', { name: 'Add' }));
    expect(await page().findByText('Added xz-utils and zstd, but Debian’s package list couldn’t be reached to validate them')).toBeInTheDocument();
  });

  it('has no Software for an archived agent', async () => {
    renderTabs(PATH, { routes, detail: { ...goAgentDetail, archived: true } });
    await page().findByText(/What it does/);
    expect(page().queryByRole('button', { name: 'Software' })).toBeNull();
  });
});

describe('About: security', () => {
  const NETWORK = '/api/v1/agents/' + SLUG + '/network';
  const EXEMPT = NETWORK + '/exempt';
  const network = agentNetwork as AgentNetwork;
  const exempt: AgentNetwork = { ...network, exempt: true };
  const LIMITS = /^Remove all network limits for this agent/;

  async function open(extra: Record<string, unknown> = {}) {
    const user = userEvent.setup();
    const h = renderTabs(PATH, { routes: { ...routes, ['GET ' + NETWORK]: network, ...extra } });
    await page().findByText(/What it does/);
    await user.click(page().getByRole('button', { name: 'Security' }));
    await page().findByText('For every agent');
    return { user, h };
  }

  const lastPost = (h: { calls: { method: string; url: string; json: unknown }[] }, url: string) => h.calls.filter((c) => c.method === 'POST' && c.url === url).at(-1)?.json;

  // Guards the split between the two lists: the team's hosts are changed on the Org page, so a Remove on one
  // here, a missing link to Org, Network, or the agent's own host shown without its Remove fails this.
  it('shows the team’s hosts read-only and the agent’s own additions removable (agent_network.json)', async () => {
    await open();
    expect(page().getByRole('heading', { name: 'Where this agent’s commands can connect' })).toBeInTheDocument();
    expect(page().getByText('deb.debian.org')).toBeInTheDocument();
    expect(page().getByText('Permanent')).toBeInTheDocument();
    expect(page().queryByRole('button', { name: 'Remove deb.debian.org' })).toBeNull();
    expect(page().getByRole('link', { name: 'Change the team’s list in Org, Network' })).toHaveAttribute('href', '/org/network');
    expect(page().getByText('Only for Engineering lead')).toBeInTheDocument();
    expect(page().getByText('api.github.com')).toBeInTheDocument();
    expect(page().getByRole('button', { name: 'Remove api.github.com' })).toBeInTheDocument();
    expect(page().getByText('An entry covers every name under it. Changes apply at once.')).toBeInTheDocument();
    expect(page().getByRole('switch', { name: LIMITS })).not.toBeChecked();
    expect(page().queryByText('No network limits')).toBeNull();
  });

  // Guards the add path: only the new host goes up, as NetworkAdd, and the page shows the list the server
  // answers with. A body carrying the whole list, or a page that ignores the answer, fails this.
  it('allows a host for this agent and shows the list the server returns', async () => {
    const added: AgentNetwork = { ...network, own: ['api.github.com', 'uploads.example.com'] };
    const { user, h } = await open({ ['POST ' + NETWORK]: added });
    await user.type(page().getByLabelText('Allow a host'), 'uploads.example.com');
    await user.click(page().getByRole('button', { name: 'Allow' }));
    expect(await page().findByText('uploads.example.com')).toBeInTheDocument();
    expect(lastPost(h, NETWORK)).toEqual({ host: 'uploads.example.com' });
    expect(page().getByRole('button', { name: 'Remove uploads.example.com' })).toBeInTheDocument();
    expect(page().getByLabelText('Allow a host')).toHaveValue('');
  });

  // Guards the refusal path: a 409 for a host already listed must reach the person in a banner and keep what
  // they typed. A swallowed error, or a field cleared as if the host were added, fails this.
  it('shows the server’s refusal of a host already listed in a banner', async () => {
    const { user } = await open({ ['POST ' + NETWORK]: () => reply(409, { error: 'api.github.com is already on this agent’s list', who: 'you' }) });
    await user.type(page().getByLabelText('Allow a host'), 'api.github.com');
    await user.click(page().getByRole('button', { name: 'Allow' }));
    const banner = (await page().findByText('api.github.com is already on this agent’s list')).closest('.kv-banner');
    expect(banner).toHaveClass('kv-banner--danger');
    expect(banner).toHaveTextContent('Check the host, then try again.');
    expect(page().getByLabelText('Allow a host')).toHaveValue('api.github.com');
  });

  // Guards Remove: it must send a DELETE for that one host and show the emptied list. A Remove that sends
  // nothing, or a list that keeps the row after the server dropped it, fails this.
  it('removes one of the agent’s own hosts', async () => {
    const { user, h } = await open({ ['DELETE ' + NETWORK + '/api.github.com']: { ...network, own: [] } });
    await user.click(page().getByRole('button', { name: 'Remove api.github.com' }));
    expect(await page().findByText('No additions.')).toBeInTheDocument();
    expect(page().queryByText('api.github.com')).toBeNull();
    expect(h.count('DELETE', NETWORK + '/api.github.com')).toBe(1);
  });

  // Guards against lifting every limit in one click: the switch must only ask, Cancel must send nothing, and
  // only the dialog's Remove limits posts exempt:true (the body the server's golden has). A switch that posts at
  // once, or a Cancel that posts, fails this.
  it('asks before removing the limits, and only Remove limits sends it', async () => {
    const { user, h } = await open({ ['POST ' + EXEMPT]: exempt });
    const limits = page().getByRole('switch', { name: LIMITS });
    expect(limits).toHaveAccessibleName(/Its commands and fetches can then send anything, of any size, anywhere/);
    await user.click(limits);
    let dialog = await screen.findByRole('dialog');
    expect(within(dialog).getByText('Remove all network limits for Engineering lead?')).toBeInTheDocument();
    expect(within(dialog).getByText('Its commands and fetches can then send anything, of any size, anywhere, and Kivali will not stop or count it.')).toBeInTheDocument();
    expect(within(dialog).getByRole('button', { name: 'Cancel' })).toHaveClass('kv-btn--secondary');
    expect(within(dialog).getByRole('button', { name: 'Remove limits' })).toHaveClass('kv-btn--danger');
    expect(limits).not.toBeChecked();
    await user.click(within(dialog).getByRole('button', { name: 'Cancel' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    expect(h.count('POST', EXEMPT)).toBe(0);
    expect(limits).not.toBeChecked();

    await user.click(limits);
    dialog = await screen.findByRole('dialog');
    await user.click(within(dialog).getByRole('button', { name: 'Remove limits' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    expect(lastPost(h, EXEMPT)).toEqual(agentNetworkExempt);
    expect(h.count('POST', EXEMPT)).toBe(1);
    expect(page().getByRole('switch', { name: LIMITS })).toBeChecked();
    expect(page().getByText('No network limits')).toBeInTheDocument();
  });

  // Guards the way back: putting the limits back is safe, so it must post exempt:false at once with no dialog.
  // A confirmation in the way, or a switch that stays on after the server answered, fails this.
  it('puts the limits back at once, without asking', async () => {
    const { user, h } = await open({ ['GET ' + NETWORK]: exempt, ['POST ' + EXEMPT]: network });
    const limits = page().getByRole('switch', { name: LIMITS });
    expect(limits).toBeChecked();
    await user.click(limits);
    await waitFor(() => expect(page().getByRole('switch', { name: LIMITS })).not.toBeChecked());
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(lastPost(h, EXEMPT)).toEqual({ exempt: false });
    expect(page().queryByText('No network limits')).toBeNull();
  });

  // Guards the warning at the top: an agent with no limits must say so, in a warning banner (danger is for
  // failures, and this was the person's choice), before anything else on the pane. A missing banner, or one
  // in another tone, fails this.
  it('warns at the top when the agent has no network limits', async () => {
    await open({ ['GET ' + NETWORK]: exempt });
    const banner = page().getByText('No network limits').closest('.kv-banner');
    expect(banner).toHaveClass('kv-banner--warning');
    expect(banner).toHaveTextContent('This agent’s commands and fetches can send anything, of any size, anywhere. Kivali does not stop or count it.');
    expect(banner?.parentElement?.firstElementChild).toBe(banner);
  });

  // Guards a failed switch: the switch must keep showing the server's state and the refusal must be visible,
  // not hidden behind a dialog left open. A switch that shows on after a 500, or no banner, fails this.
  it('keeps the switch as the server has it when lifting the limits fails', async () => {
    const { user } = await open({ ['POST ' + EXEMPT]: () => reply(500, { error: 'the network settings could not be saved', who: 'whoever runs this Kivali server' }) });
    await user.click(page().getByRole('switch', { name: LIMITS }));
    await user.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Remove limits' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    const banner = (await page().findByText('the network settings could not be saved')).closest('.kv-banner');
    expect(banner).toHaveTextContent('whoever runs this Kivali server');
    expect(page().getByRole('switch', { name: LIMITS })).not.toBeChecked();
    expect(page().queryByText('No network limits')).toBeNull();
  });

  // Guards the archive: an archived agent has no shell, so a Security view there would offer settings for
  // commands that never run. A Security button on an archived agent, or ?view=security opening the pane
  // anyway, fails this.
  it('has no Security for an archived agent', async () => {
    const h = renderTabs(PATH + '?view=security', { routes, detail: { ...goAgentDetail, archived: true } });
    await page().findByText(/What it does/);
    expect(page().queryByRole('button', { name: 'Security' })).toBeNull();
    expect(page().getByRole('button', { name: 'Role' })).toHaveAttribute('aria-pressed', 'true');
    expect(h.count('GET', NETWORK)).toBe(0);
  });
});

describe('About: the view in the address', () => {
  // Guards links into a view, such as Org, Network's link to an agent with no network limits: a page load
  // with ?view=security must land on Security, not Role. A param that is ignored fails this.
  it('opens the view ?view= names', async () => {
    renderTabs(PATH + '?view=security', { routes: { ...routes, ['GET /api/v1/agents/' + SLUG + '/network']: agentNetwork } });
    expect(await page().findByText('For every agent')).toBeInTheDocument();
    expect(page().getByRole('button', { name: 'Security' })).toHaveAttribute('aria-pressed', 'true');
    expect(page().getByRole('switch', { name: /^Remove all network limits for this agent/ })).toBeInTheDocument();
  });

  // Guards the fallback: a value About has no view for must open Role rather than an empty pane. A page
  // with nothing pressed, or no document, fails this.
  it('opens Role for a view it does not have', async () => {
    renderTabs(PATH + '?view=nothing', { routes });
    expect(await page().findByText('What it does · updated 2 days ago')).toBeInTheDocument();
    expect(page().getByRole('button', { name: 'Role' })).toHaveAttribute('aria-pressed', 'true');
  });

  // Guards the buttons once the view lives in the address: each must still switch the pane, Role included,
  // from a page that loaded on another view. A button stuck behind the param fails this.
  it('switches views with the buttons from a page that opened on one', async () => {
    const user = userEvent.setup();
    renderTabs(PATH + '?view=habits', { routes });
    expect(await page().findByText('How it works · 1 habit · updated 2 days ago')).toBeInTheDocument();
    await user.click(page().getByRole('button', { name: 'Memory' }));
    expect(await page().findByText('What it remembers · 12 notes · updated 2 days ago')).toBeInTheDocument();
    expect(page().getByRole('button', { name: 'Memory' })).toHaveAttribute('aria-pressed', 'true');
    await user.click(page().getByRole('button', { name: 'Role' }));
    expect(await page().findByText('What it does · updated 2 days ago')).toBeInTheDocument();
    expect(page().getByRole('button', { name: 'Role' })).toHaveAttribute('aria-pressed', 'true');
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
