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
  // /agents/{slug}/integrations; the connected list shows each integration's status and the agent's access in
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
    expect(within(rows[1]!).getByText('Off')).toBeInTheDocument();
    expect(within(rows[1]!).getByText('Invalid: Blogs lists a choice the form no longer offers')).toHaveClass('kv-text--danger');

    const available = page().getByRole('region', { name: 'Not connected' });
    const other = within(available).getByRole('link', { name: /Weather/ });
    expect(other).toHaveAttribute('href', '/org/integrations/weather');
    expect(within(other).getByText('Pending')).toHaveClass('kv-badge--signal');
  });

  // Guards which connection the agent uses, for an integration that takes several (agent_integrations.json's Git row):
  // the row reads "Git (Company GitLab)", and a row without a connection label reads the integration's name alone. A
  // label dropped would leave the owner unable to tell the agent's GitLab access from GitHub access.
  it('names the connection after the integration when it takes several', async () => {
    renderTabs(PATH, { routes: { ['GET ' + API]: agentIntegrations } });
    const connected = await page().findByRole('region', { name: 'Connected to Engineering lead' });
    const git = within(connected).getByRole('link', { name: /Git/ });
    expect(within(git).getByText('Git (Company GitLab)')).toBeInTheDocument();
    expect(within(git).getByText('Repositories: eng/*')).toBeInTheDocument();
    expect(within(connected).getByText('HubSpot')).toBeInTheDocument();
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
