import { screen, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { goAgentDetail } from '../../state/fixtures/transcript';
import { pastChats } from './tabsFixtures';
import { SLUG, renderTabs } from './tabsHarness';

const PATH = '/agents/' + SLUG + '/chats';
const API = '/api/v1/agents/' + SLUG + '/chats';
const page = () => within(screen.getByRole('main'));

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('Past chats: the list', () => {
  it('lists the chats newest first, under their month, with dates, messages and tokens', async () => {
    renderTabs(PATH, { routes: { ['GET ' + API]: pastChats } });
    const september = await page().findByRole('region', { name: 'September' });
    const rows = within(september).getAllByRole('link');
    expect(rows.map((r) => r.querySelector('.kv-row-title')?.textContent)).toEqual(['Hosting quotes and the order', 'Login fixes for the web app']);
    expect(within(september).getByText('12 to 20 Sept · 212 messages · ~188k tokens')).toBeInTheDocument();
    expect(within(september).getByText('2 to 10 Sept · 40 messages · ~62k tokens')).toBeInTheDocument();
    const august = page().getByRole('region', { name: 'August' });
    expect(within(august).getByText('18 Aug to 1 Sept · 1 message · ~900 tokens')).toBeInTheDocument();
    expect(rows[0]).toHaveAttribute('href', '/agents/' + SLUG + '/chats/c3');
  });

  it('leads with the current chat', async () => {
    renderTabs(PATH, { routes: { ['GET ' + API]: pastChats } });
    const current = await page().findByRole('link', { name: /Current chat/ });
    expect(current).toHaveAttribute('href', '/agents/' + SLUG);
    expect(current).toHaveTextContent(/~\d+k tokens so far|Open the chat with/);
    const all = page().getAllByRole('link', { name: /Current chat|Hosting|Login|Release 1/ });
    expect(all[0]).toBe(current);
  });

  it('says there are none yet', async () => {
    renderTabs(PATH, { routes: { ['GET ' + API]: { chats: [] } } });
    expect(await page().findByText('No past chats yet')).toBeInTheDocument();
  });

  it('leaves out the current chat for an archived agent', async () => {
    renderTabs(PATH, { routes: { ['GET ' + API]: pastChats }, detail: { ...goAgentDetail, archived: true } });
    await page().findByRole('region', { name: 'September' });
    expect(page().queryByRole('link', { name: /Current chat/ })).toBeNull();
  });

  it('says what failed and who can fix it', async () => {
    renderTabs(PATH, { routes: { ['GET ' + API]: () => new Response(JSON.stringify({ error: 'The chats could not be read.', who: 'Whoever runs this server.' }), { status: 500 }) } });
    expect(await page().findByText('The chats could not be read.')).toBeInTheDocument();
  });
});
