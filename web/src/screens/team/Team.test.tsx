import { act, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { AgentsResponse } from '../../api/types.gen';
import { goMe, goSnapshot } from '../../state/fixtures';
import { goAgents } from '../agent/tabsFixtures';
import { renderTabs, withAgent } from '../agent/tabsHarness';

const API = '/api/v1/agents';

const agents: AgentsResponse = {
  agents: [
    { ...(goAgents.agents[0] as AgentsResponse['agents'][number]), state: 'running' },
    { slug: 'chief-of-staff', name: 'Chief of Staff', role_title: 'Chief of Staff', icon: 'compass', reports_to: 'ceo', depth: 1, model: 'claude-opus-5-5', model_label: 'Opus 5.5', effort: 'medium', context_pct: 41, state: 'waiting', created: '2026-09-01T09:30:00Z' },
  ],
  archived: [
    { slug: 'grant-writer', name: 'Grant writer', role_title: 'Writes grant applications', icon: 'file-text', reports_to: 'chief-of-staff', depth: 0, model: 'claude-opus-5-5', model_label: 'Opus 5.5', effort: 'high', context_pct: 0, state: 'idle', created: '2026-08-01T09:30:00Z', archived_at: '2026-09-03T12:00:00Z' },
  ],
};

const page = () => within(screen.getByRole('main'));

afterEach(() => {
  vi.unstubAllGlobals();
});

/** From 960 (the default here) the page draws one group per manager; below it, one indented list. */
function screenWide(matches: boolean) {
  vi.stubGlobal('matchMedia', (q: string) => ({ matches, media: q, addEventListener: () => {}, removeEventListener: () => {} }));
}

async function open(snapshot = goSnapshot, response: AgentsResponse = agents) {
  screenWide(true);
  const h = renderTabs('/team', { snapshot, routes: { ['GET ' + API]: response } });
  await page().findByRole('region', { name: 'Reports to you' });
  return h;
}

describe('Team: the org by reporting line', () => {
  it('starts with the person and nests each manager’s reports under them', async () => {
    await open();
    const top = page().getByRole('region', { name: 'Reports to you' });
    expect(within(top).getByRole('link', { name: /^Chief of Staff/ })).toBeInTheDocument();
    const cos = page().getByRole('region', { name: 'Reports to Chief of Staff' });
    expect(top).toContainElement(cos);
    expect(within(cos).getByRole('link', { name: /Engineering lead/ })).toBeInTheDocument();
    const hw = page().getByRole('region', { name: 'Reports to Engineering lead' });
    expect(cos).toContainElement(hw);
    expect(within(hw).getByRole('link', { name: /Buyer/ })).toBeInTheDocument();
    expect(within(hw).getByRole('link', { name: /tester/ })).toBeInTheDocument();
    // The person is the root, not a link.
    expect(top.querySelector('.kv-avatar--person')).not.toBeNull();
    expect(page().queryByRole('link', { name: goMe.user.name })).toBeNull();
  });

  it('links each agent to its chat', async () => {
    await open();
    expect(page().getByRole('link', { name: /Engineering lead/ })).toHaveAttribute('href', '/agents/engineering-lead');
  });

  it('shows each agent’s live state from the snapshot', async () => {
    await open();
    const state = (name: RegExp) => within(page().getByRole('link', { name })).getByRole('status');
    expect(state(/Engineering lead/)).toHaveAccessibleName('Working');
    expect(state(/^Chief of Staff/)).toHaveAccessibleName('Waiting on tasks');
    expect(state(/Buyer/)).toHaveAccessibleName('Needs help');
    expect(state(/tester/)).toHaveAccessibleName('Stopped after repeated failures');
    // The state is part of the row link's name, so a screen reader hears it with the agent.
    expect(page().getByRole('link', { name: /Engineering lead/ })).toHaveAccessibleName(/Working/);
  });

  it('says a held agent (waiting with no background tasks) was stopped by you', async () => {
    await open(withAgent(goSnapshot, 'chief-of-staff', { state: 'waiting', waiting_tasks: 0 }));
    expect(within(page().getByRole('link', { name: /^Chief of Staff/ })).getByRole('status')).toHaveAccessibleName('Stopped by you');
  });

  it('follows the org stream', async () => {
    const h = await open();
    h.snapshot(withAgent(goSnapshot, 'engineering-lead', { state: 'idle', context_pct: 40 }));
    expect(within(await page().findByRole('link', { name: /Engineering lead/ })).getByRole('status')).toHaveAccessibleName('Idle');
    expect(within(page().getByRole('link', { name: /Engineering lead/ })).queryByText('40%')).toBeNull();
  });

  it('counts the team in one line, leaving out the zero parts', async () => {
    await open();
    expect(page().getByText('4 agents · 1 working · 2 needs help')).toBeInTheDocument();
  });

  it('shows the model and effort in mono beside each agent', async () => {
    await open();
    expect(within(page().getByRole('link', { name: /Engineering lead/ })).getByText('Opus 5.5 · high')).toBeInTheDocument();
    expect(within(page().getByRole('link', { name: /^Chief of Staff/ })).getByText('Opus 5.5 · medium')).toBeInTheDocument();
  });
});

describe('Team on a phone', () => {
  it('is one list of the reporting lines, the person at the root and each agent after its manager', async () => {
    screenWide(false);
    renderTabs('/team', { routes: { ['GET ' + API]: agents } });
    const list = await page().findByRole('region', { name: 'Reporting lines' });
    expect(page().queryByRole('region', { name: 'Reports to you' })).toBeNull();
    expect(within(list).getByText('You')).toBeInTheDocument();
    expect(within(list).queryByRole('link', { name: new RegExp(goMe.user.name) })).toBeNull();
    const names = within(list)
      .getAllByRole('link')
      .map((a) => a.querySelector('.kv-row-title')?.textContent ?? a.textContent);
    expect(names).toEqual(['Chief of Staff', 'Engineering lead', 'Buyer', 'tester']);
    // Each level sits one step further in than its manager.
    const indent = (name: RegExp) => within(list).getByRole('link', { name }).closest('.app-team-indent')?.className;
    expect(indent(/Chief of Staff/)).toContain('app-team-indent-1');
    expect(indent(/Engineering lead/)).toContain('app-team-indent-2');
    expect(indent(/Buyer/)).toContain('app-team-indent-3');
  });
});

describe('Team: the summaries follow membership, not every snapshot', () => {
  it('reads the agents again when someone joins or leaves, and not when a state changes', async () => {
    const h = await open();
    expect(h.count('GET', API)).toBe(1);
    h.snapshot(withAgent(goSnapshot, 'engineering-lead', { state: 'idle', context_pct: 12 }));
    h.snapshot(withAgent(goSnapshot, 'chief-of-staff', { state: 'running' }));
    await act(async () => {});
    expect(h.count('GET', API)).toBe(1);
    // An offboard: the tester leaves the snapshot.
    h.snapshot({ ...goSnapshot, agents: goSnapshot.agents.filter((a) => a.slug !== 'tester') });
    await waitFor(() => expect(h.count('GET', API)).toBe(2));
    expect(page().queryByRole('link', { name: /tester/ })).toBeNull();
  });
});

describe('Team: a long chat', () => {
  it('shows a soft context percentage from 80 and nothing below', async () => {
    await open();
    // engineering-lead sits at 82 in the golden snapshot, the Chief of Staff at 41.
    expect(within(page().getByRole('link', { name: /Engineering lead/ })).getByText('82%')).toBeInTheDocument();
    expect(within(page().getByRole('link', { name: /^Chief of Staff/ })).queryByText(/%/)).toBeNull();
  });

  it('starts at exactly 80', async () => {
    const at80 = withAgent(withAgent(goSnapshot, 'engineering-lead', { context_pct: 79 }), 'chief-of-staff', { context_pct: 80 });
    await open(at80);
    expect(within(page().getByRole('link', { name: /^Chief of Staff/ })).getByText('80%')).toBeInTheDocument();
    expect(within(page().getByRole('link', { name: /Engineering lead/ })).queryByText('79%')).toBeNull();
  });
});

describe('Team: archived agents', () => {
  it('folds them into a card at the foot, each linking to its chat', async () => {
    const user = userEvent.setup();
    await open();
    const card = (await page().findByText('Archived')).closest('details');
    expect(card).not.toBeNull();
    expect(card).not.toHaveAttribute('open');
    expect(within(card as HTMLElement).getByText('1 agent · their files are kept')).toBeInTheDocument();
    await user.click(within(card as HTMLElement).getByText('Archived'));
    expect(card).toHaveAttribute('open');
    const row = within(card as HTMLElement).getByRole('link', { name: /Grant writer/ });
    expect(row).toHaveAttribute('href', '/agents/grant-writer');
    expect(row).toHaveTextContent('Offboarded 3 Sept · reported to Chief of Staff');
  });

  it('draws no card when nothing is archived', async () => {
    await open(goSnapshot, { ...agents, archived: [] });
    expect(page().queryByText('Archived')).toBeNull();
  });
});

describe('Team: an org with no agents', () => {
  it('invites the first hire', async () => {
    renderTabs('/team', { snapshot: { ...goSnapshot, agents: [] }, routes: { ['GET ' + API]: { agents: [], archived: [] } } });
    expect(await page().findByText('No agents yet')).toBeInTheDocument();
    expect(page().getByText('Every team starts with one.')).toBeInTheDocument();
    expect(page().queryByRole('region', { name: 'Reports to you' })).toBeNull();
  });
});
