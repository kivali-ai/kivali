import { screen, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { agentIntegrations } from './tabsFixtures';
import { SLUG, renderTabs } from './tabsHarness';

const PATH = '/agents/' + SLUG + '/integrations';
const API = '/api/v1/agents/' + SLUG + '/integrations';
const page = () => within(screen.getByRole('main'));

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('Integrations tab', () => {
  // Guards the tab's reading of agent_integrations.json and its route: the Integrations tab is the current one at
  // /agents/{slug}/integrations; the connected list shows each integration's state and the agent's access in
  // words, with access that no longer fits in the danger tone; the rest follow under Not connected; and every row
  // opens the integration's page under Org, where access is changed. A row in the wrong list, a lost summary or a
  // link anywhere else fails here.
  it('lists what the agent is connected to and what it is not, each linking to the integration', async () => {
    renderTabs(PATH, { routes: { ['GET ' + API]: agentIntegrations } });
    const connected = await page().findByRole('region', { name: 'Connected to Engineering lead' });
    expect(screen.getByRole('tab', { name: 'Integrations' })).toHaveAttribute('aria-selected', 'true');

    const rows = within(connected).getAllByRole('link');
    expect(rows.map((r) => r.getAttribute('href'))).toEqual(['/org/integrations/hubspot', '/org/integrations/crm', '/org/integrations/git']);
    expect(within(rows[0]!).getByText('HubSpot')).toBeInTheDocument();
    expect(within(rows[0]!).getByText('Connected')).toHaveClass('kv-badge--success');
    expect(within(rows[0]!).getByText('Blogs: Marketing · May publish and edit')).toBeInTheDocument();
    expect(within(rows[1]!).getByText('Disabled')).toBeInTheDocument();
    expect(within(rows[1]!).getByText('Invalid: Blogs lists a choice the form no longer offers')).toHaveClass('kv-text--danger');

    const available = page().getByRole('region', { name: 'Not connected' });
    const other = within(available).getByRole('link', { name: /Weather/ });
    expect(other).toHaveAttribute('href', '/org/integrations/weather');
    expect(within(other).getByText('Waiting for approval')).toHaveClass('kv-badge--signal');
  });

  // Guards the row's title now that an integration has one connection (agent_integrations.json's Git row carries no
  // connection label): it is the integration's display name alone, with the agent's access beside it. A title that
  // still appended a connection name, or showed the integration's name key, fails here.
  it('titles each row with the integration’s display name alone', async () => {
    renderTabs(PATH, { routes: { ['GET ' + API]: agentIntegrations } });
    const connected = await page().findByRole('region', { name: 'Connected to Engineering lead' });
    const git = within(connected).getByRole('link', { name: /Git/ });
    expect(git.querySelector('.kv-row-title')?.textContent).toBe('Git');
    expect(within(git).getByText('Repositories: eng/*')).toBeInTheDocument();
    expect(within(connected).getByText('HubSpot')).toBeInTheDocument();
  });

  // Guards whether the agent's calls can go through now (usable): the server reads it from the integration's
  // one connection, so a Git connection that needs its secrets again reads not usable on this agent's row while Git is
  // connected, and a usable row says nothing of it. A row showing the state alone would hide that this agent's calls
  // fail.
  it('says when the agent cannot use a connected integration', async () => {
    renderTabs(PATH, { routes: { ['GET ' + API]: agentIntegrations } });
    const connected = await page().findByRole('region', { name: 'Connected to Engineering lead' });
    const git = within(connected).getByRole('link', { name: /Git/ });
    expect(within(git).getByText('Connected')).toHaveClass('kv-badge--success');
    expect(within(git).getByText('Not usable until its requirements are met')).toHaveClass('kv-text--danger');
    const hubspot = within(connected).getByRole('link', { name: /HubSpot/ });
    expect(within(hubspot).queryByText('Not usable until its requirements are met')).toBeNull();
  });

  // Guards an agent with no access anywhere: the connected list says so in words instead of drawing an empty
  // container, and the integrations it could be given still follow.
  it('says when the agent is connected to nothing', async () => {
    renderTabs(PATH, { routes: { ['GET ' + API]: { connected: [], available: agentIntegrations.available } } });
    expect(await page().findByText('No integrations connected.')).toBeInTheDocument();
    expect(page().getByRole('link', { name: /Weather/ })).toBeInTheDocument();
  });

  // Guards an agent connected to everything: the second list says nothing is left rather than showing empty.
  it('says when every installed integration is connected', async () => {
    renderTabs(PATH, { routes: { ['GET ' + API]: { connected: agentIntegrations.connected, available: [] } } });
    expect(await page().findByText('Every installed integration is connected.')).toBeInTheDocument();
  });

  // Guards a team with nothing installed: one line instead of two empty lists, each with its own empty sentence.
  it('says when no integration is installed', async () => {
    renderTabs(PATH, { routes: { ['GET ' + API]: { connected: [], available: [] } } });
    expect(await page().findByText('No integrations installed.')).toBeInTheDocument();
    expect(page().queryByRole('region', { name: 'Not connected' })).toBeNull();
  });

  // Guards a failed read: the server's sentence shows instead of an endless skeleton.
  it('says what failed and who can fix it', async () => {
    renderTabs(PATH, { routes: { ['GET ' + API]: () => new Response(JSON.stringify({ error: 'The integrations could not be read.', who: 'Whoever runs this server.' }), { status: 500 }) } });
    expect(await page().findByText('The integrations could not be read.')).toBeInTheDocument();
  });
});
