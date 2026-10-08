import { act, fireEvent, render, screen, within } from '@testing-library/react';
import { createMemoryRouter } from 'react-router';
import { RouterProvider } from 'react-router/dom';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ApiError, apiGet } from '../../api/client';
import type { Graph as GraphData, GraphNode, NodeRow } from '../../api/types.gen';
import { TooltipProvider } from '../../ds';
import { desktop, flush, goOrg, setOrg, useOrgStub } from '../work/testOrg';
import { goGraph, goGraphNode } from './fixtures';
import { Graph, SEARCH_DEBOUNCE_MS } from './Graph';

vi.mock('../../api/client', async (importOriginal) => {
  const real = await importOriginal<typeof import('../../api/client')>();
  return { ...real, apiGet: vi.fn() };
});

vi.mock('../../state/OrgProvider', () => ({ useOrg: () => useOrgStub(), usePersonName: () => 'You' }));

const getMock = vi.mocked(apiGet);
const clock = () => Date.parse('2026-09-20T18:00:00Z');

const moreNodes: NodeRow[] = ['a', 'b', 'c'].map((n, i) => ({
  id: 'engineering-lead/more-' + n,
  kind: 'decision',
  type: 'artifact',
  title: 'More ' + n,
  status: 'current',
  flagged: false,
  version: i + 1,
}));

interface Served {
  graph?: GraphData;
  node?: GraphNode;
}

function serve({ graph = goGraph, node = goGraphNode }: Served = {}) {
  getMock.mockImplementation(async (path: string) => {
    const url = new URL(path, 'http://x');
    if (url.pathname === '/api/v1/graph') {
      if (url.searchParams.has('owner')) {
        const owner = graph.owners.find((o) => o.slug === url.searchParams.get('owner'));
        return structuredClone({ ...graph, owners: owner ? [{ ...owner, nodes: moreNodes, has_more: false }] : [] });
      }
      return structuredClone(graph);
    }
    if (url.pathname === '/api/v1/graph/nodes/engineering-lead/changelog') return structuredClone(node);
    throw new ApiError(404, 'not found', 'Nobody');
  });
}

async function renderGraph(served: Served = {}, path = '/graph') {
  serve(served);
  setOrg(goOrg());
  const router = createMemoryRouter([{ path: '/graph', element: <Graph clock={clock} /> }], { basename: '/', initialEntries: [path] });
  const utils = render(
    <TooltipProvider>
      <RouterProvider router={router} />
    </TooltipProvider>,
  );
  await flush();
  return { ...utils, router };
}

const calls = () => getMock.mock.calls.map((c) => c[0]);

beforeEach(() => {
  vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout', 'setInterval', 'clearInterval', 'Date'] });
  vi.setSystemTime(new Date('2026-09-20T18:00:00Z'));
  desktop(true);
  getMock.mockReset();
});

afterEach(() => {
  vi.useRealTimers();
});

describe('the list', () => {
  it('reads the graph as readouts and owners folded with counts', async () => {
    await renderGraph();
    expect(screen.getByText('1,204')).toBeInTheDocument();
    for (const label of ['nodes', 'flagged', 'problems']) expect(screen.getAllByText(label, { exact: false }).length).toBeGreaterThan(0);
    expect(screen.getByRole('heading', { name: 'Engineering lead' })).toBeInTheDocument();
    expect(screen.getByText('12 nodes')).toBeInTheDocument();
    // The golden's Unowned section is empty, so it is not drawn as "0 nodes".
    expect(screen.queryByRole('heading', { name: 'Unowned' })).not.toBeInTheDocument();
    expect(screen.queryByText('0 nodes')).not.toBeInTheDocument();
  });

  it('calls the section for slug "" Unowned when it holds nodes', async () => {
    const graph = structuredClone(goGraph);
    const unowned = graph.owners.find((o) => o.slug === '');
    if (!unowned) throw new Error('fixture lost the Unowned section');
    unowned.count = 1;
    unowned.nodes = [{ id: 'loose/note', kind: 'artifact', type: 'artifact', title: 'A loose note', status: 'current', flagged: false, version: 1 }];
    await renderGraph({ graph });
    expect(screen.getByRole('heading', { name: 'Unowned' })).toBeInTheDocument();
    expect(screen.getByText('1 node')).toBeInTheDocument();
  });

  it('shows the first page of nodes, one row each, with a short fact on desktop', async () => {
    await renderGraph();
    const phase = screen.getByRole('button', { name: /engineering-lead\/changelog/ });
    expect(phase).toHaveAttribute('aria-expanded', 'false');
    expect(phase).toHaveTextContent('Every release has a changelog');
    expect(phase).toHaveTextContent('v3');
    expect(screen.getByRole('button', { name: /engineering-lead\/bad/ })).toHaveTextContent('problem');
  });

  it('drops the short fact on a phone so the chip never clips', async () => {
    desktop(false);
    await renderGraph();
    expect(screen.getByRole('button', { name: /engineering-lead\/changelog/ })).not.toHaveTextContent('v3');
  });

  it('says the index is stale, quietly', async () => {
    await renderGraph();
    expect(screen.getByRole('status')).toHaveTextContent('Showing the last index while it refreshes');
    expect(within(screen.getByRole('status')).queryByRole('button')).not.toBeInTheDocument();
  });

  it('omits the stale notice when the index is fresh', async () => {
    await renderGraph({ graph: { ...goGraph, stale: false } });
    expect(screen.queryByText('Showing the last index while it refreshes')).not.toBeInTheDocument();
  });

  it('invites the first node when the graph is empty', async () => {
    await renderGraph({ graph: { readouts: { nodes: 0, flagged: 0, problems: 0 }, owners: [], stale: false } });
    expect(screen.getByRole('heading', { name: 'Nothing in the graph yet' })).toBeInTheDocument();
  });

  it('says so when a search matches nothing', async () => {
    await renderGraph({ graph: { ...goGraph, owners: [] } }, '/graph?q=zzz');
    expect(screen.getByText('No nodes match those filters.')).toBeInTheDocument();
    expect(screen.getByLabelText('Find a node')).toHaveValue('zzz');
  });

  it('says what happened when the graph cannot load', async () => {
    getMock.mockRejectedValue(new ApiError(503, 'the knowledge graph has not been indexed yet', 'Whoever runs this Kivali server can look into it.'));
    setOrg(goOrg());
    const router = createMemoryRouter([{ path: '/graph', element: <Graph clock={clock} /> }], { basename: '/', initialEntries: ['/graph'] });
    render(
      <TooltipProvider>
        <RouterProvider router={router} />
      </TooltipProvider>,
    );
    await flush();
    expect(screen.getByRole('alert')).toHaveTextContent('The knowledge graph has not been indexed yet.');
  });
});

describe('Show all', () => {
  it('fetches the rest of one owner from the offset and appends it', async () => {
    await renderGraph();
    const all = screen.getByRole('button', { name: 'Show all 12' });
    await act(async () => {
      all.click();
    });
    await flush();
    expect(calls()).toContain('/api/v1/graph?owner=engineering-lead&offset=2&limit=10');
    for (const n of ['a', 'b', 'c']) expect(screen.getByRole('button', { name: new RegExp('engineering-lead/more-' + n) })).toBeInTheDocument();
    // The first page stays, and there is nothing more to show.
    expect(screen.getByRole('button', { name: /engineering-lead\/changelog/ })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /Show all/ })).not.toBeInTheDocument();
  });

  it('carries the active filters', async () => {
    await renderGraph({}, '/graph?flagged=1');
    await act(async () => {
      screen.getByRole('button', { name: 'Show all 12' }).click();
    });
    await flush();
    expect(calls()).toContain('/api/v1/graph?flagged=1&owner=engineering-lead&offset=2&limit=10');
  });
});

describe('filters and search', () => {
  it('toggles Flagged and Problems into the query', async () => {
    const { router } = await renderGraph();
    const flagged = screen.getByRole('button', { name: 'Flagged · 3' });
    const problems = screen.getByRole('button', { name: 'Problems · 2' });
    expect(flagged).toHaveAttribute('aria-pressed', 'false');
    expect(flagged).toHaveClass('kv-btn--ghost');
    await act(async () => {
      flagged.click();
    });
    await flush();
    expect(calls().at(-1)).toBe('/api/v1/graph?flagged=1');
    expect(screen.getByRole('button', { name: 'Flagged · 3' })).toHaveAttribute('aria-pressed', 'true');
    expect(screen.getByRole('button', { name: 'Flagged · 3' })).toHaveClass('kv-btn--secondary');
    await act(async () => {
      problems.click();
    });
    await flush();
    expect(calls().at(-1)).toBe('/api/v1/graph?flagged=1&problems=1');
    expect(router.state.location.search).toBe('?flagged=1&problems=1');
    await act(async () => {
      screen.getByRole('button', { name: 'Flagged · 3' }).click();
    });
    await flush();
    expect(calls().at(-1)).toBe('/api/v1/graph?problems=1');
  });

  it('waits for typing to settle before searching, and puts the search in the URL', async () => {
    const { router } = await renderGraph();
    const before = getMock.mock.calls.length;
    fireEvent.change(screen.getByLabelText('Find a node'), { target: { value: 'release' } });
    await act(async () => {
      vi.advanceTimersByTime(SEARCH_DEBOUNCE_MS - 1);
    });
    await flush();
    expect(getMock.mock.calls.length).toBe(before);
    fireEvent.change(screen.getByLabelText('Find a node'), { target: { value: 'release notes' } });
    await act(async () => {
      vi.advanceTimersByTime(SEARCH_DEBOUNCE_MS - 1);
    });
    expect(getMock.mock.calls.length).toBe(before);
    await act(async () => {
      vi.advanceTimersByTime(1);
    });
    await flush();
    expect(router.state.location.search).toBe('?q=release+notes');
    expect(calls().at(-1)).toBe('/api/v1/graph?q=release+notes');
  });

  it('starts from ?q= and drops it when cleared', async () => {
    const { router } = await renderGraph({}, '/graph?q=phase');
    expect(screen.getByLabelText('Find a node')).toHaveValue('phase');
    expect(calls()[0]).toBe('/api/v1/graph?q=phase');
    fireEvent.change(screen.getByLabelText('Find a node'), { target: { value: '' } });
    await act(async () => {
      vi.advanceTimersByTime(SEARCH_DEBOUNCE_MS);
    });
    await flush();
    expect(router.state.location.search).toBe('');
    expect(calls().at(-1)).toBe('/api/v1/graph');
  });
});

describe('node detail', () => {
  it('expands inline on desktop: fields as a definition list, the text, a version chip', async () => {
    await renderGraph();
    const row = screen.getByRole('button', { name: /engineering-lead\/changelog/ });
    await act(async () => {
      row.click();
    });
    await flush();
    expect(calls()).toContain('/api/v1/graph/nodes/engineering-lead/changelog');
    expect(row).toHaveAttribute('aria-expanded', 'true');
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    const term = screen.getByText('Condition', { selector: 'dt' });
    expect(term.nextElementSibling).toHaveTextContent('until the next release');
    expect(screen.getByText('Rests on', { selector: 'dt' }).nextElementSibling).toHaveTextContent('chief-of-staff/release-policy@2');
    expect(screen.getByText('v1')).toBeInTheDocument();
    expect(screen.getByText('The text of this version is no longer stored. The version stays on record.')).toBeInTheDocument();
    expect(screen.getByText(/Every release has a changelog\./)).toBeInTheDocument();
    // Fields with nothing in them are left out.
    expect(screen.queryByText('Payload', { selector: 'dt' })).not.toBeInTheDocument();
    // Clicking the row again folds it.
    await act(async () => {
      row.click();
    });
    expect(row).toHaveAttribute('aria-expanded', 'false');
    expect(screen.queryByText('Condition', { selector: 'dt' })).not.toBeInTheDocument();
  });

  it('opens a dialog on a phone', async () => {
    desktop(false);
    await renderGraph();
    await act(async () => {
      screen.getByRole('button', { name: /engineering-lead\/changelog/ }).click();
    });
    await flush();
    const d = within(screen.getByRole('dialog'));
    expect(d.getByRole('heading', { name: 'engineering-lead/changelog' })).toBeInTheDocument();
    expect(d.getByText('Condition', { selector: 'dt' }).nextElementSibling).toHaveTextContent('until the next release');
    expect(d.getByText('v1')).toBeInTheDocument();
    fireEvent.click(d.getByRole('button', { name: 'Close' }));
    await flush();
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  });

  it('shows a mono note instead of text when the version is gone', async () => {
    const { body_md: _text, ...noText } = goGraphNode;
    void _text;
    await renderGraph({ node: noText });
    await act(async () => {
      screen.getByRole('button', { name: /engineering-lead\/changelog/ }).click();
    });
    await flush();
    expect(screen.getByText('The text of this version is no longer stored. The version stays on record.')).toBeInTheDocument();
    expect(screen.queryByText(/Every release has a changelog\./)).not.toBeInTheDocument();
  });

  it('says what happened when a node cannot be read', async () => {
    serve();
    getMock.mockImplementation(async (path: string) => {
      if (path.startsWith('/api/v1/graph/nodes/')) throw new ApiError(404, 'no such node', 'It may have been removed.');
      return structuredClone(goGraph);
    });
    setOrg(goOrg());
    const router = createMemoryRouter([{ path: '/graph', element: <Graph clock={clock} /> }], { basename: '/', initialEntries: ['/graph'] });
    render(
      <TooltipProvider>
        <RouterProvider router={router} />
      </TooltipProvider>,
    );
    await flush();
    await act(async () => {
      screen.getByRole('button', { name: /engineering-lead\/changelog/ }).click();
    });
    await flush();
    expect(screen.getByRole('alert')).toHaveTextContent('No such node.');
  });
});
