import { act, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Chat } from '../../api/types.gen';
import { fakeIntersectionObserver } from '../../lib/fakeIntersectionObserver';
import type { FakeObserver } from '../../lib/fakeIntersectionObserver';
import { identityColorFor } from '../../lib/agentIdentity';
import { emptyChat } from '../../state/fixtures/transcript';
import { renderAgent, snapshotWith } from './chatHarness';

const ROLE_LINE = 'Owns the product from plan to release.';
const idle: Chat = { ...emptyChat, fill: { ...emptyChat.fill, pct: 40 } };

let observers: FakeObserver[];

beforeEach(() => {
  window.localStorage.clear();
  const fake = fakeIntersectionObserver();
  observers = fake.instances;
  vi.stubGlobal('IntersectionObserver', fake.Observer);
});
afterEach(() => {
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

function desktop() {
  vi.stubGlobal('matchMedia', (q: string) => ({ matches: q.includes('min-width'), media: q, addEventListener: () => {}, removeEventListener: () => {} }));
}

async function loaded() {
  await screen.findByRole('list', { name: 'Chat with Engineering lead' });
  await screen.findByText(ROLE_LINE);
}

function header(): HTMLElement {
  const el = document.querySelector<HTMLElement>('.app-agent-head');
  if (!el) throw new Error('no agent header');
  return el;
}

function sentinel(): HTMLElement {
  const el = document.querySelector<HTMLElement>('.app-agent-sentinel');
  if (!el) throw new Error('no sentinel');
  return el;
}

/** The observer watching the sentinel at the top of the body. */
function watcher(): FakeObserver {
  const o = observers.find((x) => !x.disconnected && x.targets.includes(sentinel()));
  if (!o) throw new Error('nothing observes the sentinel');
  return o;
}

function scrollPast() {
  act(() => watcher().fire([{ target: sentinel(), isIntersecting: false, top: -400 }]));
}

function scrollBack() {
  act(() => watcher().fire([{ target: sentinel(), isIntersecting: true, top: 12 }]));
}

describe('AgentPage: the Background tab counts the tasks still out', () => {
  const tab = () => screen.getByRole('tab', { name: /Background/ });

  it('follows the org snapshot, not the count the agent was read with', async () => {
    // The agent's own read says 2 (agent_detail.json) and is made once; the snapshot is what moves.
    const h = renderAgent({ chat: idle, snapshot: snapshotWith('waiting', 40, undefined, 3) });
    await loaded();
    expect(tab()).toHaveTextContent(/^Background3$/);
    h.snapshot(snapshotWith('waiting', 40, undefined, 1));
    expect(tab()).toHaveTextContent(/^Background1$/);
    h.snapshot(snapshotWith('idle', 40, undefined, 0));
    expect(tab()).toHaveTextContent(/^Background0$/);
    h.snapshot(snapshotWith('running', 40, undefined, 2));
    expect(tab()).toHaveTextContent(/^Background2$/);
  });

  it('shows the count the agent was read with for an agent the tree does not hold', async () => {
    const snapshot = snapshotWith('idle');
    renderAgent({ chat: idle, snapshot: { ...snapshot, agents: snapshot.agents.filter((a) => a.slug !== 'engineering-lead') } });
    await loaded();
    expect(tab()).toHaveTextContent(/^Background2$/);
  });
});

describe('AgentPage: the pinned header', () => {
  it('pins the header at phone width and condenses it to one slim row once the body is scrolled', async () => {
    renderAgent({ chat: idle, snapshot: snapshotWith('running') });
    await loaded();
    expect(header()).toHaveClass('is-pinned');
    expect(header()).not.toHaveClass('is-condensed');
    expect(screen.getByText(ROLE_LINE)).toBeInTheDocument();

    scrollPast();
    expect(header()).toHaveClass('is-pinned', 'is-condensed');
    // The role line hides; the slim row has the way back, the name, the compact state, the context reading,
    // New chat as an icon button, and the menu.
    expect(screen.queryByText(ROLE_LINE)).toBeNull();
    const row = within(header());
    expect(row.getByRole('button', { name: 'Back to Team' })).toBeInTheDocument();
    expect(row.getByText('Engineering lead', { selector: '.app-agent-name' }).tagName).toBe('P');
    expect(row.getByRole('status', { name: 'Working' })).toBeInTheDocument();
    expect(row.getByText('40%')).toBeInTheDocument();
    expect(row.getByRole('button', { name: 'New chat' })).toHaveClass('kv-btn--icon');
    expect(row.getByRole('button', { name: 'More' })).toBeInTheDocument();
    // Still one h1: the phone header's.
    expect(screen.getAllByRole('heading', { level: 1 })).toHaveLength(1);

    scrollBack();
    expect(header()).not.toHaveClass('is-condensed');
    expect(screen.getByText(ROLE_LINE)).toBeInTheDocument();
    expect(within(header()).queryByRole('button', { name: 'Back to Team' })).toBeNull();
    expect(screen.getAllByRole('heading', { level: 1 })).toHaveLength(1);
  });

  it('pins the header from 960 and never condenses it there', async () => {
    desktop();
    renderAgent({ chat: idle, snapshot: snapshotWith('running') });
    await loaded();
    expect(header()).toHaveClass('is-pinned');
    scrollPast();
    expect(header()).toHaveClass('is-pinned');
    expect(header()).not.toHaveClass('is-condensed');
    expect(screen.getByText(ROLE_LINE)).toBeInTheDocument();
    const h1s = screen.getAllByRole('heading', { level: 1 }).filter((h) => !h.classList.contains('app-phone-title'));
    expect(h1s).toEqual([screen.getByText('Engineering lead', { selector: '.app-agent-name' })]);
  });

  it('keeps the tabs in the page and working while condensed', async () => {
    const user = userEvent.setup();
    renderAgent({ chat: idle, snapshot: snapshotWith('idle') });
    await loaded();
    scrollPast();
    const tabs = screen.getByRole('tablist');
    expect(within(tabs).getAllByRole('tab')).toHaveLength(4);
    await user.click(within(tabs).getByRole('tab', { name: /Background/ }));
    await waitFor(() => expect(screen.getByRole('tab', { name: /Background/ })).toHaveAttribute('aria-selected', 'true'));
    // The header stays pinned (and is watched again) on the other tabs, which share the page.
    expect(header()).toHaveClass('is-pinned');
    scrollBack();
    expect(header()).not.toHaveClass('is-condensed');
    scrollPast();
    expect(header()).toHaveClass('is-condensed');
    expect(screen.getByRole('tablist')).toBeInTheDocument();
  });

  it('opens New chat from the slim row, and makes it the primary action past the threshold', async () => {
    const user = userEvent.setup();
    renderAgent({ chat: { ...idle, fill: { ...idle.fill, pct: 85 } }, snapshot: snapshotWith('idle', 85) });
    await loaded();
    scrollPast();
    const row = within(header());
    expect(await row.findByText('85%')).toBeInTheDocument();
    const newChat = row.getByRole('button', { name: 'New chat' });
    expect(newChat).toHaveClass('kv-btn--primary');
    await user.click(newChat);
    expect(await screen.findByRole('dialog', { name: 'Start a new chat with Engineering lead?' })).toBeInTheDocument();
  });

  it('keeps the reading position when the header changes height: scrolls by how far the body moved', async () => {
    renderAgent({ chat: idle, snapshot: snapshotWith('idle') });
    await loaded();
    const scrollBy = vi.spyOn(window, 'scrollBy').mockImplementation(() => {});
    const rect = (top: number) => ({ top, bottom: top, left: 0, right: 0, width: 0, height: 0, x: 0, y: top, toJSON: () => ({}) }) as DOMRect;
    // Condensing lifts everything below the header by 64px.
    vi.spyOn(sentinel(), 'getBoundingClientRect').mockReturnValueOnce(rect(-400)).mockReturnValueOnce(rect(-464));
    scrollPast();
    expect(scrollBy).toHaveBeenCalledWith(0, -64);
    // At the foot of the page the browser clamps the scroll itself: nothing moved, nothing to do.
    scrollBy.mockClear();
    vi.spyOn(sentinel(), 'getBoundingClientRect').mockReturnValueOnce(rect(0)).mockReturnValueOnce(rect(0));
    scrollBack();
    expect(scrollBy).not.toHaveBeenCalled();
  });

  it('stays expanded without an IntersectionObserver', async () => {
    vi.stubGlobal('IntersectionObserver', undefined);
    renderAgent({ chat: idle });
    await loaded();
    expect(header()).toHaveClass('is-pinned');
    expect(header()).not.toHaveClass('is-condensed');
  });
});

describe('AgentPage: the agent wears one face', () => {
  it('draws the header and its chat messages in the identity colour the sidebar uses', async () => {
    desktop();
    renderAgent({ chat: idle, snapshot: snapshotWith('idle', 40) });
    await loaded();
    const want = 'var(--identity-' + identityColorFor('engineering-lead') + ')';
    const faces = screen.getAllByRole('img', { name: 'Engineering lead' });
    expect(within(header()).getByRole('img', { name: 'Engineering lead' }).style.background).toBe(want);
    for (const face of faces) expect(face.style.background).toBe(want);
  });
});
