import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import type { ReactNode } from 'react';
import { useSearchParams } from 'react-router';
import { apiGet } from '../../api/client';
import type { ApiError } from '../../api/client';
import type { Graph as GraphData, GraphNode, GraphOwner, NodeRow as NodeRowData } from '../../api/types.gen';
import { useFrameChrome } from '../../app/chrome';
import { PageTitle } from '../../app/PageTitle';
import { BREAKPOINT_TABBAR, Banner, Button, Card, Dialog, EmptyState, NodeRow, Readouts, Skeleton, Text, TextField, mediaQuery } from '../../ds';
import type { NodeChipProps, NodeKind, NodeStatus } from '../../ds';
import { identityColorFor } from '../../lib/agentIdentity';
import { useMediaQuery } from '../../lib/useMediaQuery';
import { asSentence } from '../../state/home';
import { asApiError, useSlow } from '../work/parts';
import { NodeDetail, fieldRows } from './NodeDetail';
import '../../styles/graph.css';

/** Typing settles this long before the search goes into the URL and out to the server. */
export const SEARCH_DEBOUNCE_MS = 250;

/** The wire's kind for NodeChip's glyph: decisions and requirements have their own; everything else is an artifact. */
function chipKind(kind: string): NodeKind {
  return kind === 'decision' || kind === 'requirement' ? kind : 'artifact';
}

/** Flagged and problem nodes take the warning look, superseded and withdrawn ones the struck-through look. */
function chipStatus(n: NodeRowData): NodeStatus {
  if (n.flagged || n.problem) return 'flagged';
  if (n.status === 'superseded' || n.status === 'withdrawn') return 'superseded';
  return 'active';
}

function chipFor(n: NodeRowData, ownerSlug: string, ownerName: string): NodeChipProps {
  return { id: n.id, kind: chipKind(n.kind), summary: n.title, status: chipStatus(n), owner: identityColorFor(ownerSlug), ownerName };
}

function shortFor(n: NodeRowData): string {
  if (n.problem) return 'problem';
  return n.version > 0 ? 'v' + n.version : '';
}

function nodeWord(n: number): string {
  return n === 1 ? '1 node' : n + ' nodes';
}

/** The query the server takes; the same text keys the client's paging state. */
export function graphQuery(q: string, flagged: boolean, problems: boolean): string {
  const p = new URLSearchParams();
  if (q) p.set('q', q);
  if (flagged) p.set('flagged', '1');
  if (problems) p.set('problems', '1');
  return p.toString();
}

/** A node id as a path under /graph/nodes/: the slashes stay, the rest is escaped. */
function nodePath(id: string): string {
  return id.split('/').map(encodeURIComponent).join('/');
}

interface Page {
  nodes: NodeRowData[];
  hasMore: boolean;
}

function GraphSkeleton() {
  return (
    <div className="app-graph-loading" aria-busy="true" aria-label="Loading Graph">
      {[0, 1, 2].map((i) => (
        <div key={i} className="app-graph-skeleton">
          <Skeleton width="30%" />
        </div>
      ))}
    </div>
  );
}

interface OwnerSectionProps {
  owner: GraphOwner;
  page: Page | undefined;
  open: boolean;
  desktop: boolean;
  openId: string | null;
  node: GraphNode | null;
  nodeError: ApiError | null;
  loadingMore: boolean;
  now: number;
  onToggle(id: string): void;
  onShowAll(owner: GraphOwner): void;
}

function OwnerSection({ owner, page, open, desktop, openId, node, nodeError, loadingMore, now, onToggle, onShowAll }: OwnerSectionProps) {
  const nodes = page?.nodes ?? owner.nodes;
  const hasMore = page ? page.hasMore : owner.has_more;
  const name = owner.name || 'Unowned';
  return (
    <Card collapsible defaultOpen={open} title={name} meta={nodeWord(owner.count)}>
      <div className="app-graph-rows">
        {nodes.map((n) => {
          const expanded = desktop && openId === n.id;
          return (
            <div key={n.id}>
              <NodeRow
                node={chipFor(n, owner.slug, name)}
                short={shortFor(n)}
                expanded={expanded}
                onToggle={() => onToggle(n.id)}
                compact={!desktop}
                {...(expanded ? { fields: node ? fieldRows(node.fields) : nodeError ? [] : [['Loading', <Skeleton key="loading" width="60%" />] as [string, ReactNode]] } : {})}
              />
              {expanded && (
                <div className="app-graph-inline">
                  <NodeDetail node={node} error={nodeError} now={now} bare />
                </div>
              )}
            </div>
          );
        })}
        {hasMore && (
          <div className="app-graph-more">
            <Button variant="ghost" size="sm" loading={loadingMore} disabled={loadingMore} onClick={() => onShowAll(owner)}>
              {'Show all ' + owner.count}
            </Button>
          </div>
        )}
      </div>
    </Card>
  );
}

export interface GraphProps {
  /** The clock dates read against. Tests pass a fixed one. */
  clock?: () => number;
}

/** Graph: a compact list built for thousands of nodes. Search, Flagged and Problems, owners folded with counts. */
export function Graph({ clock = Date.now }: GraphProps) {
  useFrameChrome({ title: 'Graph' });
  const [params, setParams] = useSearchParams();
  const q = params.get('q') ?? '';
  const flagged = params.get('flagged') === '1';
  const problems = params.get('problems') === '1';
  const filtering = q !== '' || flagged || problems;
  const desktop = useMediaQuery(mediaQuery(BREAKPOINT_TABBAR));
  const now = useMemo(clock, [clock]);

  // ---- The list: one fetch per query, then per-owner pages appended ----
  const [data, setData] = useState<GraphData | null>(null);
  const [error, setError] = useState<ApiError | null>(null);
  const [pages, setPages] = useState<Record<string, Page>>({});
  const [loadingMore, setLoadingMore] = useState<string | null>(null);
  const seq = useRef(0);
  const query = graphQuery(q, flagged, problems);

  const load = useCallback(async () => {
    const mine = ++seq.current;
    try {
      const next = await apiGet<GraphData>('/api/v1/graph' + (query ? '?' + query : ''));
      if (mine !== seq.current) return;
      setData(next);
      setPages({});
      setError(null);
    } catch (err) {
      if (mine !== seq.current) return;
      setError(asApiError(err));
    }
  }, [query]);

  useEffect(() => {
    void load();
    return () => {
      // A response that lands after unmount, or after the query changed, is dropped.
      seq.current++;
    };
  }, [load]);

  const showAll = async (owner: GraphOwner) => {
    const loaded = pages[owner.slug]?.nodes ?? owner.nodes;
    const mine = seq.current;
    setLoadingMore(owner.slug);
    try {
      const p = new URLSearchParams(query);
      p.set('owner', owner.slug);
      p.set('offset', String(loaded.length));
      p.set('limit', String(Math.max(owner.count - loaded.length, 1)));
      const next = await apiGet<GraphData>('/api/v1/graph?' + p.toString());
      if (mine !== seq.current) return;
      const more = next.owners.find((o) => o.slug === owner.slug);
      setPages((prev) => ({ ...prev, [owner.slug]: { nodes: [...loaded, ...(more?.nodes ?? [])], hasMore: more?.has_more ?? false } }));
    } catch (err) {
      if (mine !== seq.current) return;
      setError(asApiError(err));
    } finally {
      setLoadingMore(null);
    }
  };

  // ---- Search: what is typed goes into the URL once it settles ----
  const [text, setText] = useState(q);
  useEffect(() => {
    setText(q);
  }, [q]);
  useEffect(() => {
    const next = text.trim();
    if (next === q) return;
    const t = setTimeout(() => {
      setParams(
        (prev) => {
          const out = new URLSearchParams(prev);
          if (next) out.set('q', next);
          else out.delete('q');
          return out;
        },
        { replace: true },
      );
    }, SEARCH_DEBOUNCE_MS);
    return () => clearTimeout(t);
  }, [text, q, setParams]);

  const toggleFilter = (key: 'flagged' | 'problems') => {
    setParams(
      (prev) => {
        const out = new URLSearchParams(prev);
        if (out.get(key) === '1') out.delete(key);
        else out.set(key, '1');
        return out;
      },
      { replace: true },
    );
  };

  // ---- One node open at a time: inline on desktop, a dialog on phone ----
  const [openId, setOpenId] = useState<string | null>(null);
  const [node, setNode] = useState<GraphNode | null>(null);
  const [nodeError, setNodeError] = useState<ApiError | null>(null);
  useEffect(() => {
    if (openId === null) return;
    let live = true;
    setNode(null);
    setNodeError(null);
    apiGet<GraphNode>('/api/v1/graph/nodes/' + nodePath(openId))
      .then((n) => live && setNode(n))
      .catch((err: unknown) => live && setNodeError(asApiError(err)));
    return () => {
      live = false;
    };
  }, [openId]);
  const toggleNode = (id: string) => setOpenId((cur) => (cur === id ? null : id));

  const slow = useSlow(data === null && !error);
  const readouts = data?.readouts;
  // An owner with nothing to show (the server's empty Unowned section) is not drawn as "0 nodes".
  const owners = useMemo(() => (data ? data.owners.filter((o) => o.count > 0 || o.nodes.length > 0) : []), [data]);
  const openNodeName = useMemo(() => {
    if (!data || openId === null) return '';
    for (const o of data.owners) {
      const hit = (pages[o.slug]?.nodes ?? o.nodes).find((n) => n.id === openId);
      if (hit) return hit.title;
    }
    return '';
  }, [data, pages, openId]);

  return (
    <div className="app-graph">
      <div className="app-graph-top">
        <PageTitle>Graph</PageTitle>
        {readouts && (
          <Readouts
            items={[
              { n: readouts.nodes.toLocaleString('en-US'), label: 'nodes' },
              { n: readouts.flagged.toLocaleString('en-US'), label: 'flagged' },
              { n: readouts.problems.toLocaleString('en-US'), label: 'problems' },
            ]}
          />
        )}
      </div>
      <div className="app-graph-tools">
        <div className="app-graph-search">
          <TextField label="Find a node" type="search" value={text} onChange={(e) => setText(e.target.value)} placeholder="changelog, login, req/pricing" />
        </div>
        <div className="app-graph-filters">
          <Button size="sm" variant={flagged ? 'secondary' : 'ghost'} aria-pressed={flagged} onClick={() => toggleFilter('flagged')}>
            {readouts ? 'Flagged · ' + readouts.flagged : 'Flagged'}
          </Button>
          <Button size="sm" variant={problems ? 'secondary' : 'ghost'} aria-pressed={problems} onClick={() => toggleFilter('problems')}>
            {readouts ? 'Problems · ' + readouts.problems : 'Problems'}
          </Button>
        </div>
      </div>
      {error && (
        <Banner
          tone="danger"
          title={asSentence(error.message)}
          action={
            <Button size="sm" variant="secondary" onClick={() => void load()}>
              Try again
            </Button>
          }
        >
          {error.who}
        </Banner>
      )}
      {data?.stale && <Banner tone="info" title="Showing the last index while it refreshes" />}
      {data === null && !error && slow && <GraphSkeleton />}
      {data &&
        (owners.length === 0 ? (
          data.readouts.nodes === 0 ? (
            <EmptyState title="Nothing in the graph yet">Agents publish decisions and requirements here as they work, and they appear as a list.</EmptyState>
          ) : (
            <Text as="p" variant="body" tone="muted">
              No nodes match{filtering ? ' those filters' : ''}.
            </Text>
          )
        ) : (
          <div className="app-graph-owners">
            {owners.map((o, i) => (
              <OwnerSection
                key={o.slug || 'unowned'}
                owner={o}
                page={pages[o.slug]}
                open={filtering || owners.length === 1 || i === 0}
                desktop={desktop}
                openId={openId}
                node={node}
                nodeError={nodeError}
                loadingMore={loadingMore === o.slug}
                now={now}
                onToggle={toggleNode}
                onShowAll={(owner) => void showAll(owner)}
              />
            ))}
          </div>
        ))}
      {!desktop && openId !== null && (
        <Dialog
          open
          onOpenChange={(open) => {
            if (!open) setOpenId(null);
          }}
          title={openId}
          {...(openNodeName ? { description: openNodeName } : {})}
        >
          <NodeDetail node={node} error={nodeError} now={now} />
        </Dialog>
      )}
    </div>
  );
}
