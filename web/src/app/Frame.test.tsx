import { act, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { setUnauthenticatedHandler } from '../api/client';
import { goSetup } from '../screens/setup/fixtures';
import { busySnapshot, quietSnapshot, releaseOnlySnapshot } from '../state/fixtures';
import { jsonResponse, renderApp } from './test-utils';

function sidebar() {
  return within(screen.getByRole('complementary', { name: 'Sidebar' }));
}

/**
 * An exact accessible name, ignoring the space after each comma: jsdom's name computation trims the
 * screen-reader-only ", " separators (browsers keep the space).
 */
function named(name: string): RegExp {
  const escaped = name.replace(/[.*+?^${}()|[\]\\]/g, '\\$&').replace(/, /g, ',\\s*');
  return new RegExp('^' + escaped + '$');
}

async function ready() {
  // The tree link appears once /api/v1/snapshot has been applied. Scoped to the sidebar: Team, the page some
  // of these tests open on, lists the same agents.
  const bar = await screen.findByRole('complementary', { name: 'Sidebar' });
  await within(bar).findByRole('link', { name: /Chief of Staff/ });
}

describe('Frame', () => {
  beforeEach(() => {
    window.localStorage.clear();
    document.documentElement.removeAttribute('data-theme');
  });
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it('renders the five destinations and the team tree from a snapshot', async () => {
    renderApp('/team', busySnapshot);
    await ready();

    const main = within(sidebar().getByRole('navigation', { name: 'Main' }));
    for (const label of ['Home', 'Team', 'Work', 'Graph', 'Org']) {
      expect(main.getByRole('link', { name: new RegExp('^' + label) })).toBeInTheDocument();
    }
    expect(main.getByRole('link', { name: /^Team/ })).toHaveAttribute('aria-current', 'page');

    const tree = within(sidebar().getByRole('group', { name: 'Team' }));
    // The person is the root: a label, not a link.
    expect(tree.getByText('Maya')).toBeInTheDocument();
    expect(tree.getByText('you')).toBeInTheDocument();
    expect(tree.queryByRole('link', { name: /Maya/ })).toBeNull();
    for (const name of ['Chief of Staff', 'Engineering lead', 'Test runner', 'Support lead', 'Garden advisor']) {
      expect(tree.getByRole('link', { name: new RegExp(name) })).toBeInTheDocument();
    }
    expect(tree.getByRole('link', { name: /Test runner/ })).toHaveAttribute('href', '/agents/test-runner');
  });

  it('draws Home as the canvas does: running dots beside the label, the count as the signal pill', async () => {
    renderApp('/team', busySnapshot);
    await ready();
    const home = sidebar().getByRole('link', { name: /^Home/ });
    // The count sits in NavItem's count slot with attention, which kivali.css draws as the signal pill.
    const count = home.querySelector('.kv-nav-count');
    expect(count).toHaveClass('is-attention');
    expect(count?.lastChild?.textContent).toBe('3');
    // The running dots sit inside the label, after the word, not in the count slot.
    const dots = within(home).getByRole('status', { name: 'Working' });
    expect(home.querySelector('.kv-nav-label')).toContainElement(dots);
    expect(count).not.toContainElement(dots);
    expect(home).toHaveAccessibleName(named('Home, Working, 3'));
  });

  it('marks the active destination with the raised tile the design system styles', async () => {
    renderApp('/work', busySnapshot);
    await ready();
    const work = sidebar().getByRole('link', { name: /^Work/ });
    expect(work).toHaveClass('kv-nav', 'is-active');
    expect(work).toHaveAttribute('aria-current', 'page');
    expect(sidebar().getByRole('link', { name: /^Home/ })).not.toHaveAttribute('aria-current');
  });

  it('names each tree item with its state', async () => {
    renderApp('/team', busySnapshot);
    await ready();
    const tree = within(sidebar().getByRole('group', { name: 'Team' }));
    expect(tree.getByRole('link', { name: named('Chief of Staff, Working, 86%') })).toBeInTheDocument();
    expect(tree.getByRole('link', { name: named('Engineering lead, Waiting on tasks') })).toBeInTheDocument();
    expect(tree.getByRole('link', { name: named('Support lead, Needs help') })).toBeInTheDocument();
    expect(tree.getByRole('link', { name: 'Garden advisor' })).toBeInTheDocument();
  });

  it('shows no count and no running dot on a quiet org', async () => {
    renderApp('/team', quietSnapshot);
    await ready();
    const home = sidebar().getByRole('link', { name: /^Home/ });
    expect(within(home).queryByText(/\d/)).toBeNull();
    expect(within(home).queryByRole('status')).toBeNull();
  });

  it('shows the running dot when the release engine runs, with no count', async () => {
    renderApp('/team', releaseOnlySnapshot);
    await ready();
    const home = sidebar().getByRole('link', { name: /^Home/ });
    expect(within(home).getByRole('status', { name: 'Working' })).toBeInTheDocument();
    expect(within(home).queryByText(/\d/)).toBeNull();
  });

  it('shows context fill in the tree only at 80 percent and above (ContextCount threshold)', async () => {
    const pct: Record<string, number> = { 'chief-of-staff': 86, 'engineering-lead': 80, 'test-runner': 79, 'support-lead': 50, advisor: 7 };
    const snapshot = { ...busySnapshot, agents: busySnapshot.agents.map((a) => ({ ...a, context_pct: pct[a.slug] ?? 0 })) };
    renderApp('/team', snapshot);
    await ready();
    const tree = within(sidebar().getByRole('group', { name: 'Team' }));
    expect(tree.getByText('86%')).toBeInTheDocument();
    expect(tree.getByText('80%')).toBeInTheDocument();
    expect(tree.queryByText('79%')).toBeNull();
    expect(tree.queryByText('50%')).toBeNull();
    // An idle agent below the threshold draws an empty count slot, not an empty wrapper.
    expect(tree.getByRole('link', { name: 'Test runner' }).querySelector('.kv-nav-count')).toBeNull();
  });

  it('shows each agent state as compact dots, and nothing for an idle agent', async () => {
    renderApp('/team', busySnapshot);
    await ready();
    const tree = within(sidebar().getByRole('group', { name: 'Team' }));
    expect(within(tree.getByRole('link', { name: /Chief of Staff/ })).getByRole('status', { name: 'Working' })).toBeInTheDocument();
    expect(within(tree.getByRole('link', { name: /Engineering lead/ })).getByRole('status', { name: 'Waiting on tasks' })).toBeInTheDocument();
    expect(within(tree.getByRole('link', { name: /Support lead/ })).getByRole('status', { name: 'Needs help' })).toBeInTheDocument();
    expect(within(tree.getByRole('link', { name: /Garden advisor/ })).queryByRole('status')).toBeNull();
  });

  it('names a held agent (waiting with no tasks) "Stopped by you", not "Waiting on tasks"', async () => {
    const held = { ...busySnapshot, agents: busySnapshot.agents.map((a) => (a.slug === 'test-runner' ? { ...a, state: 'waiting' as const, waiting_tasks: 0 } : a)) };
    renderApp('/team', held);
    await ready();
    const tree = within(sidebar().getByRole('group', { name: 'Team' }));
    expect(within(tree.getByRole('link', { name: /Test runner/ })).getByRole('status', { name: 'Stopped by you' })).toBeInTheDocument();
    expect(within(tree.getByRole('link', { name: /Engineering lead/ })).getByRole('status', { name: 'Waiting on tasks' })).toBeInTheDocument();
  });

  it('follows /org/stream snapshots', async () => {
    const { stream } = renderApp('/team', quietSnapshot);
    await ready();
    expect(stream.started).toBe(true);
    act(() => stream.emit('snapshot', busySnapshot));
    const home = sidebar().getByRole('link', { name: /^Home/ });
    await waitFor(() => expect(home).toHaveAccessibleName(named('Home, Working, 3')));
    expect(sidebar().getByRole('link', { name: /Garden advisor/ })).toBeInTheDocument();
  });

  it('opens the account menu upward with the theme choices and Sign out', async () => {
    const user = userEvent.setup();
    const { signOut } = renderApp('/team', quietSnapshot);
    await ready();

    await user.click(sidebar().getByRole('button', { name: 'Account' }));
    const menu = await screen.findByRole('menu');
    expect(menu).toHaveAttribute('data-side', 'top');
    const items = within(menu).getAllByRole('menuitem');
    expect(items.map((i) => i.textContent?.replace('Current', ''))).toEqual(['Match device', 'Light', 'Dark', 'Sign out']);
    // The current choice carries a check.
    expect(within(screen.getByRole('menuitem', { name: /Match device/ })).getByRole('img', { name: 'Current' })).toBeInTheDocument();
    expect(within(screen.getByRole('menuitem', { name: /^Dark/ })).queryByRole('img', { name: 'Current' })).toBeNull();

    await user.click(screen.getByRole('menuitem', { name: /^Dark/ }));
    expect(document.documentElement.getAttribute('data-theme')).toBe('dark');
    expect(window.localStorage.getItem('kivali.theme')).toBe('dark');

    await user.click(sidebar().getByRole('button', { name: 'Account' }));
    expect(within(await screen.findByRole('menuitem', { name: /^Dark/ })).getByRole('img', { name: 'Current' })).toBeInTheDocument();

    await user.click(screen.getByRole('menuitem', { name: 'Sign out' }));
    expect(signOut).toHaveBeenCalledTimes(1);
  });

  it('posts the chosen auto-release stop', async () => {
    const user = userEvent.setup();
    const { calls } = renderApp('/team', busySnapshot);
    await ready();
    const group = within(sidebar().getByRole('radiogroup', { name: 'Auto-release' }));
    expect(group.getByRole('radio', { name: '5m' })).toBeChecked();

    await user.click(group.getByRole('radio', { name: '2m' }));
    const post = calls.find((c) => c.method === 'POST' && c.url === '/api/v1/auto-release');
    expect(post?.body).toBe(JSON.stringify({ value: '2m' }));
    expect(group.getByRole('radio', { name: '2m' })).toBeChecked();

    await user.click(group.getByRole('radio', { name: 'Now' }));
    expect(calls.filter((c) => c.method === 'POST').at(-1)?.body).toBe(JSON.stringify({ value: 'now' }));
  });

  it('puts the slider back and says what happened when the write fails', async () => {
    const user = userEvent.setup();
    renderApp('/team', busySnapshot, undefined, {
      'POST /api/v1/auto-release': () => new Response(JSON.stringify({ error: 'The setting was not saved.', who: 'Try again in a moment.' }), { status: 500 }),
    });
    await ready();
    const group = within(sidebar().getByRole('radiogroup', { name: 'Auto-release' }));
    await user.click(group.getByRole('radio', { name: 'Off' }));
    expect(await screen.findByText('The setting was not saved.')).toBeInTheDocument();
    expect(screen.getByText('Try again in a moment.')).toBeInTheDocument();
    expect(group.getByRole('radio', { name: '5m' })).toBeChecked();
  });

  it('names the org at the top of the sidebar, as plain text with no switcher', async () => {
    renderApp('/team', quietSnapshot);
    await ready();
    expect(sidebar().getByText('Plainsong')).toBeInTheDocument();
    expect(sidebar().getByRole('img', { name: 'Plainsong' })).toBeInTheDocument();
    expect(sidebar().queryByRole('button', { name: /Plainsong/ })).toBeNull();
  });
});

describe('Frame: an org that still needs setup', () => {
  afterEach(() => vi.unstubAllGlobals());
  const setupGets = (calls: { method: string; url: string }[]) => calls.filter((c) => c.method === 'GET' && c.url === '/api/v1/setup').length;

  it('sends Home to the wizard on a fresh install', async () => {
    renderApp('/', busySnapshot, undefined, { 'GET /api/v1/setup': { ...goSetup, step: 'welcome' } });
    expect(await screen.findByRole('heading', { level: 1, name: 'Set up your org' })).toBeInTheDocument();
    expect(screen.queryByRole('complementary', { name: 'Sidebar' })).toBeNull();
  });

  it('asks once per load and leaves a set-up org on Home', async () => {
    const user = userEvent.setup();
    const { calls } = renderApp('/', busySnapshot, undefined, { 'GET /api/v1/setup': { ...goSetup, needed: false, step: 'done' } });
    await ready();
    await waitFor(() => expect(setupGets(calls)).toBe(1));
    await user.click(sidebar().getByRole('link', { name: /^Team/ }));
    await user.click(sidebar().getByRole('link', { name: /^Home/ }));
    await within(await screen.findByRole('complementary', { name: 'Sidebar' })).findByRole('link', { name: /^Home/, current: 'page' });
    expect(setupGets(calls)).toBe(1);
    expect(screen.getByRole('complementary', { name: 'Sidebar' })).toBeInTheDocument();
  });

  it('does not ask from a page other than Home', async () => {
    const { calls } = renderApp('/team', busySnapshot, undefined, { 'GET /api/v1/setup': { ...goSetup, step: 'welcome' } });
    await ready();
    expect(setupGets(calls)).toBe(0);
    expect(screen.getByRole('complementary', { name: 'Sidebar' })).toBeInTheDocument();
  });
});

describe('Frame chrome', () => {
  beforeEach(() => window.localStorage.clear());
  afterEach(() => vi.unstubAllGlobals());

  it('shows the tab bar on a destination, with the same five items and the count on Home', async () => {
    renderApp('/team', busySnapshot);
    await ready();
    const bar = within(screen.getByRole('navigation', { name: 'Tab bar' }));
    const links = bar.getAllByRole('link');
    expect(links.map((l) => l.textContent)).toEqual(['3Home', 'Team', 'Work', 'Graph', 'Org']);
    expect(links[0]).toHaveAccessibleName('Home, 3 need you');
    expect(bar.getByRole('link', { name: /Team/ })).toHaveAttribute('aria-current', 'page');
  });

  it('puts the org mark, with no control on it, and the one phone h1 in the phone header', async () => {
    renderApp('/team', busySnapshot);
    await ready();
    const header = screen.getByRole('banner');
    expect(within(header).getByRole('img', { name: 'Plainsong' })).toBeInTheDocument();
    expect(within(header).queryByRole('button')).toBeNull();
    expect(within(header).getByRole('heading', { level: 1, name: 'Team' })).toHaveClass('app-phone-title');
  });

  it('shows no error banner when the first load is a 401 (the redirect to sign-in is under way)', async () => {
    const onUnauth = vi.fn();
    setUnauthenticatedHandler(onUnauth);
    try {
      renderApp('/team', busySnapshot, undefined, {
        'GET /api/v1/me': () => jsonResponse(401, { error: 'You are signed out.', who: 'Sign in again.' }),
      });
      await waitFor(() => expect(onUnauth).toHaveBeenCalled());
      // Let the rejected Promise.all settle.
      await act(async () => {});
      expect(screen.queryByText('You are signed out.')).toBeNull();
    } finally {
      setUnauthenticatedHandler(null);
    }
  });

  it('hides the tab bar on an agent page and shows a back link', async () => {
    const user = userEvent.setup();
    renderApp('/agents/chief-of-staff', busySnapshot);
    await ready();
    expect(screen.queryByRole('navigation', { name: 'Tab bar' })).toBeNull();
    // Desktop back link in the column, phone back button in the header.
    expect(screen.getByRole('button', { name: 'Team' })).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Back to Team' }));
    expect(await screen.findByRole('navigation', { name: 'Tab bar' })).toBeInTheDocument();
  });

  it('hides the tab bar on proposals and assignment pages', async () => {
    renderApp('/proposals/hire/engineering-lead', busySnapshot);
    await ready();
    expect(screen.queryByRole('navigation', { name: 'Tab bar' })).toBeNull();
    expect(screen.getByRole('button', { name: 'Back to Home' })).toBeInTheDocument();
  });

  it('keeps the parent destination active on a sub-page', async () => {
    renderApp('/agents/test-runner', busySnapshot);
    await ready();
    const main = within(sidebar().getByRole('navigation', { name: 'Main' }));
    expect(main.getByRole('link', { name: /^Team/ })).toHaveAttribute('aria-current', 'page');
    const tree = within(sidebar().getByRole('group', { name: 'Team' }));
    expect(tree.getByRole('link', { name: /Test runner/ })).toHaveAttribute('aria-current', 'page');
  });

  it('titles the tab Kivali on Home and Kivali · page elsewhere', async () => {
    renderApp('/', busySnapshot);
    await ready();
    expect(document.title).toBe('Kivali');
    const user = userEvent.setup();
    await user.click(sidebar().getByRole('link', { name: /^Team/ }));
    await waitFor(() => expect(document.title).toBe('Kivali · Team'));
  });

  it('widens the column for the Work board and narrows it for a chat', async () => {
    renderApp('/work', busySnapshot);
    await ready();
    expect(document.querySelector('.app-column')).toHaveClass('is-wide');
  });

  it('navigates without a page load when a design-system link is clicked', async () => {
    const user = userEvent.setup();
    renderApp('/', busySnapshot);
    await ready();
    await user.click(sidebar().getByRole('link', { name: /Engineering lead/ }));
    expect(await screen.findAllByRole('heading', { level: 1, name: 'Engineering lead' })).not.toHaveLength(0);
    expect(document.title).toBe('Kivali · Engineering lead');
  });

  it('leaves a modified click to the browser', async () => {
    const user = userEvent.setup();
    renderApp('/', busySnapshot);
    await ready();
    // Runs after the app's listener: records whether it claimed the click, then stops jsdom following the link.
    let claimed = false;
    const spy = (e: MouseEvent) => {
      claimed = e.defaultPrevented;
      e.preventDefault();
    };
    document.addEventListener('click', spy);
    await user.keyboard('{Control>}');
    await user.click(sidebar().getByRole('link', { name: /^Team/ }));
    await user.keyboard('{/Control}');
    expect(claimed).toBe(false);
    expect(document.title).toBe('Kivali');
    await user.click(sidebar().getByRole('link', { name: /^Team/ }));
    expect(claimed).toBe(true);
    document.removeEventListener('click', spy);
  });

  it('leaves hash-only, download, new-tab, middle-click and out-of-app links to the browser', async () => {
    renderApp('/', busySnapshot);
    await ready();
    const claimed: boolean[] = [];
    const spy = (e: MouseEvent) => {
      claimed.push(e.defaultPrevented);
      e.preventDefault();
    };
    document.addEventListener('click', spy);
    // Appended to body, as a Radix portal's content is: the listener is on the document, so position does not matter.
    const make = (href: string, attrs: Record<string, string> = {}) => {
      const a = document.createElement('a');
      a.setAttribute('href', href);
      for (const [k, v] of Object.entries(attrs)) a.setAttribute(k, v);
      a.textContent = 'x';
      document.body.appendChild(a);
      return a;
    };
    const click = (a: HTMLAnchorElement, init: MouseEventInit = {}) =>
      act(() => {
        a.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true, button: 0, ...init }));
      });
    try {
      click(make('#section'));
      click(make('/team', { download: '' }));
      click(make('/team', { target: '_blank' }));
      click(make('/team'), { button: 1 });
      click(make('/login'));
      click(make('https://example.com/app/team'));
      expect(claimed).toEqual([false, false, false, false, false, false]);
      expect(document.title).toBe('Kivali');
      // A plain in-app link in a portal is a soft navigation.
      click(make('/team'));
      expect(claimed.at(-1)).toBe(true);
      await waitFor(() => expect(document.title).toBe('Kivali · Team'));
    } finally {
      document.removeEventListener('click', spy);
      for (const a of document.body.querySelectorAll(':scope > a')) a.remove();
    }
  });

  it('renders the not-found screen outside the frame for an unknown path', async () => {
    renderApp('/nowhere', busySnapshot);
    expect(await screen.findByRole('heading', { name: 'This page isn’t here' })).toBeInTheDocument();
    expect(screen.queryByRole('complementary')).toBeNull();
    expect(screen.getByRole('button', { name: 'Go to Home' })).toBeInTheDocument();
    expect(document.title).toBe('Kivali · Page not found');
  });

  it('says what happened and who can fix it when the first load fails', async () => {
    renderApp('/team', busySnapshot, undefined, {
      'GET /api/v1/snapshot': () => new Response(JSON.stringify({ error: 'Kivali could not read the org.', who: 'Whoever runs this server can look into it.' }), { status: 500 }),
    });
    expect(await screen.findByText('Kivali could not read the org.')).toBeInTheDocument();
    expect(screen.getByText('Whoever runs this server can look into it.')).toBeInTheDocument();
  });
});
