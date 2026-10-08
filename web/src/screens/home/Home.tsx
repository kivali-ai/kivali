import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { Link, useHref, useNavigate, useSearchParams } from 'react-router';
import { ApiError, apiGet, apiPost, apiPostForm } from '../../api/client';
import type { AutoRelease, Home as HomeData, NeedActionResponse, NeedItem } from '../../api/types.gen';
import { useFrameChrome } from '../../app/chrome';
import type { FrameChrome } from '../../app/chrome';
import { PageTitle } from '../../app/PageTitle';
import { BREAKPOINT_TABBAR, Banner, Button, Icon, Tabs, Text, Toast, mediaQuery } from '../../ds';
import { identifierFor } from '../../lib/agentIdentity';
import { useMediaQuery } from '../../lib/useMediaQuery';
import {
  asSentence,
  effectiveSelection,
  filterQueue,
  hasRunningCountdown,
  hide,
  pruneHidden,
  refetchDecision,
  releaseAllPlan,
  routerPath,
  toggleSelected,
  unhide,
  visibleNeeds,
  visibleQueue,
} from '../../state/home';
import type { QueueFilter } from '../../state/home';
import { useOrg } from '../../state/OrgProvider';
import { History } from './History';
import { InFlight } from './InFlight';
import { Needs } from './Needs';
import type { NeedAction } from './Needs';
import { IdentityProvider, RowSkeletons } from './parts';
import { Queue } from './Queue';
import { systemClock, useNow } from './useNow';
import type { Clock } from './useNow';
import '../../styles/home.css';

/** Skeletons appear only when loading takes longer than this; faster loads just appear. */
const SKELETON_DELAY_MS = 300;
/** Toasts leave on their own: five seconds, longer when they carry a link. */
const TOAST_MS = 5000;
const TOAST_WITH_ACTION_MS = 8000;

const TABS = [
  { value: 'now', label: 'Now' },
  { value: 'history', label: 'History' },
];

function asApiError(err: unknown): ApiError {
  return err instanceof ApiError ? err : new ApiError(0, 'Something went wrong in Kivali.', 'Reload the page. If it keeps happening, whoever runs this Kivali server can look into it.');
}

function toggleIn(set: ReadonlySet<string>, id: string): ReadonlySet<string> {
  const next = new Set(set);
  if (next.has(id)) next.delete(id);
  else next.add(id);
  return next;
}

interface ToastEntry {
  id: number;
  title: string;
  link?: string;
}

function HomeToast({ entry, onDone, onOpen }: { entry: ToastEntry; onDone(id: number): void; onOpen(link: string): void }) {
  const { id, link } = entry;
  useEffect(() => {
    const t = setTimeout(() => onDone(id), link ? TOAST_WITH_ACTION_MS : TOAST_MS);
    return () => clearTimeout(t);
  }, [id, link, onDone]);
  return (
    <Toast
      tone="success"
      title={entry.title}
      onDismiss={() => onDone(id)}
      action={
        link ? (
          <Button size="sm" variant="secondary" onClick={() => onOpen(link)}>
            Open
          </Button>
        ) : undefined
      }
    />
  );
}

export interface HomeProps {
  /** The clock countdowns and times read. Tests pass a fake. */
  clock?: Clock;
}

/** Home: Needs you, In flight and Queue on the Now tab; History on its own tab (desktop) or page (phone). */
export function Home({ clock = systemClock }: HomeProps) {
  const { org, setAutoRelease } = useOrg();
  const navigate = useNavigate();
  const base = useHref('/').replace(/\/$/, '');
  const [params, setParams] = useSearchParams();
  const view = params.get('view') === 'history' ? 'history' : 'now';
  const desktop = useMediaQuery(mediaQuery(BREAKPOINT_TABBAR));

  const chrome: FrameChrome = { title: view === 'history' ? 'History' : 'Home' };
  if (view === 'history' && !desktop) chrome.back = { to: '/', label: 'Home' };
  useFrameChrome(chrome);

  const [data, setData] = useState<HomeData | null>(null);
  const [loadError, setLoadError] = useState<ApiError | null>(null);
  const [actionError, setActionError] = useState<ApiError | null>(null);
  const [slow, setSlow] = useState(false);
  const [hiddenNeeds, setHiddenNeeds] = useState<ReadonlySet<string>>(() => new Set());
  const [hiddenQueue, setHiddenQueue] = useState<ReadonlySet<string>>(() => new Set());
  const [expandedNeeds, setExpandedNeeds] = useState<ReadonlySet<string>>(() => new Set());
  const [expandedQueue, setExpandedQueue] = useState<ReadonlySet<string>>(() => new Set());
  const [selected, setSelected] = useState<ReadonlySet<string>>(() => new Set());
  const [filter, setFilter] = useState<QueueFilter>('all');
  const [toasts, setToasts] = useState<ToastEntry[]>([]);
  const toastId = useRef(0);

  // ---- Loading and the refetch decision ----
  const seq = useRef(0);
  const load = useCallback(async () => {
    const mine = ++seq.current;
    try {
      const next = await apiGet<HomeData>('/api/v1/home');
      if (mine !== seq.current) return;
      setData(next);
      setLoadError(null);
      setHiddenNeeds((h) => pruneHidden(h, next.needs.map((n) => n.id)));
      setHiddenQueue((h) => pruneHidden(h, next.queue.map((q) => q.path)));
    } catch (err) {
      if (mine !== seq.current) return;
      setLoadError(asApiError(err));
    }
  }, []);

  useEffect(() => {
    void load();
    return () => {
      // A response that lands after unmount is dropped.
      seq.current++;
    };
  }, [load]);

  const prevOrg = useRef(org);
  const armed = useRef(false);
  useEffect(() => {
    const d = refetchDecision(prevOrg.current, org, armed.current);
    prevOrg.current = org;
    armed.current = d.armed;
    if (d.refetch) void load();
  }, [org, load]);

  useEffect(() => {
    if (data) return;
    const t = setTimeout(() => setSlow(true), SKELETON_DELAY_MS);
    return () => clearTimeout(t);
  }, [data]);

  // ---- What is on screen ----
  const autoRelease: AutoRelease = org.ready ? org.autoRelease : (data?.auto_release ?? org.autoRelease);
  const goals = org.ready ? org.goals : (data?.goals ?? []);
  const readouts = org.ready ? org.readouts : (data?.readouts ?? org.readouts);
  const needs = useMemo(() => visibleNeeds(data?.needs ?? [], hiddenNeeds), [data, hiddenNeeds]);
  const queued = useMemo(() => visibleQueue(data?.queue ?? [], hiddenQueue), [data, hiddenQueue]);
  const shown = useMemo(() => filterQueue(queued, filter), [queued, filter]);
  const identify = useMemo(() => identifierFor(org.tree), [org.tree]);
  const selection = effectiveSelection(selected, shown, autoRelease);

  // Once a second only while a countdown is on screen; otherwise once a minute, for the "8m" style times.
  const now = useNow(view === 'now' && hasRunningCountdown(queued, autoRelease) ? 1000 : 60_000, clock);

  // ---- Actions: the row leaves at once and comes back if the server refuses ----
  const fail = (err: unknown) => setActionError(asApiError(err));

  const act = async (item: NeedItem, action: NeedAction) => {
    setHiddenNeeds((h) => hide(h, [item.id]));
    try {
      if (action.kind === 'close') {
        if (!item.assignment) throw new ApiError(0, 'This assignment has no number to close.', 'Whoever runs this Kivali server can look into it.');
        await apiPost('/api/v1/assignments/' + item.assignment.id + '/close', { resolution: action.resolution, outcome: action.outcome });
        return;
      }
      const form = new FormData();
      form.append('path', item.id);
      form.append('message', action.message);
      for (const f of action.files) form.append('attachments[]', f, f.name);
      const res = await apiPostForm<NeedActionResponse>('/api/v1/needs/' + action.verb, form);
      if (res?.result) {
        const id = ++toastId.current;
        setToasts((t) => [...t, { id, title: res.result ?? '', ...(res.link ? { link: res.link } : {}) }]);
      }
    } catch (err) {
      setHiddenNeeds((h) => unhide(h, [item.id]));
      fail(err);
    }
  };

  const release = async (path: string, note?: string) => {
    setHiddenQueue((h) => hide(h, [path]));
    setSelected((s) => toggleSelected(s, path, false));
    try {
      await apiPost('/api/v1/queue/release', note ? { path, note } : { path });
    } catch (err) {
      setHiddenQueue((h) => unhide(h, [path]));
      fail(err);
    }
  };

  const releaseAll = async () => {
    const plan = releaseAllPlan(selection, shown);
    setHiddenQueue((h) => hide(h, plan.paths));
    setSelected(new Set());
    try {
      await apiPost('/api/v1/queue/release-all', plan.body);
    } catch (err) {
      setHiddenQueue((h) => unhide(h, plan.paths));
      fail(err);
    }
  };

  const bounce = async (path: string, comment: string) => {
    setHiddenQueue((h) => hide(h, [path]));
    setSelected((s) => toggleSelected(s, path, false));
    try {
      await apiPost('/api/v1/queue/bounce', { path, comment });
    } catch (err) {
      setHiddenQueue((h) => unhide(h, [path]));
      fail(err);
    }
  };

  const dropToast = useCallback((id: number) => setToasts((t) => t.filter((x) => x.id !== id)), []);
  const openLink = useCallback((link: string) => void navigate(routerPath(link, base)), [navigate, base]);

  // ---- Layout ----
  const loading = data === null && !loadError;
  const nowTab = (
    <>
      {loading ? (
        slow && <LoadingSections titles={org.ready ? ['Needs you'] : ['Needs you', 'In flight', 'Queue']} />
      ) : (
        data && (
          <Needs
            items={needs}
            now={now}
            base={base}
            expanded={expandedNeeds}
            onToggle={(id) => setExpandedNeeds((s) => toggleIn(s, id))}
            onAct={(item, a) => void act(item, a)}
          />
        )
      )}
      {(org.ready || data) && <InFlight goals={goals} readouts={readouts} tree={org.tree} base={base} maxWorkers={desktop ? 3 : 2} />}
      {loading ? (
        slow && org.ready && <LoadingSections titles={['Queue']} hidden />
      ) : (
        data && (
          <Queue
            items={shown}
            total={queued.length}
            autoRelease={autoRelease}
            onAutoRelease={(v) => void setAutoRelease(v)}
            compact={!desktop}
            filter={filter}
            onFilter={setFilter}
            selection={selection}
            now={now}
            expanded={expandedQueue}
            onToggle={(p) => setExpandedQueue((s) => toggleIn(s, p))}
            onSelect={(p, on) => setSelected((s) => toggleSelected(s, p, on))}
            onRelease={(p, note) => void release(p, note)}
            onReleaseAll={() => void releaseAll()}
            onBounce={(p, c) => void bounce(p, c)}
          />
        )
      )}
      {!desktop && (
        <Link className="app-home-foot-link" to="?view=history">
          History
          <Icon name="arrow-right" />
        </Link>
      )}
    </>
  );

  return (
    <IdentityProvider value={identify}>
      <div className="app-home">
        <div className="app-home-top">
          <PageTitle>Home</PageTitle>
          {desktop && <Tabs tabs={TABS} value={view} onValueChange={(v) => setParams(v === 'history' ? { view: 'history' } : {})} />}
        </div>
        {loadError && (
          <Banner
            tone="danger"
            title={asSentence(loadError.message)}
            action={
              <Button size="sm" variant="secondary" onClick={() => void load()}>
                Try again
              </Button>
            }
          >
            {loadError.who}
          </Banner>
        )}
        {actionError && (
          <Banner tone="danger" title={asSentence(actionError.message)} onDismiss={() => setActionError(null)}>
            {actionError.who}
          </Banner>
        )}
        {view === 'history' ? <History now={now} version={org.messagesTotal} /> : nowTab}
        {toasts.length > 0 && (
          <div className="app-home-toasts">
            {toasts.map((t) => (
              <HomeToast key={t.id} entry={t} onDone={dropToast} onOpen={openLink} />
            ))}
          </div>
        )}
      </div>
    </IdentityProvider>
  );
}

/** Skeleton sections. The one under a drawn In flight is `hidden` from assistive tech: the first already says Home is loading. */
function LoadingSections({ titles, hidden = false }: { titles: string[]; hidden?: boolean }) {
  return (
    <div className="app-home-loading" {...(hidden ? { 'aria-hidden': true } : { 'aria-busy': true, 'aria-label': 'Loading Home' })}>
      {titles.map((title) => (
        <section key={title} className="app-home-section">
          <div className="app-home-head">
            <Text variant="heading" tone="muted">
              {title}
            </Text>
          </div>
          <div className="app-home-list">
            <div className="app-home-rows">
              <RowSkeletons count={2} />
            </div>
          </div>
        </section>
      ))}
    </div>
  );
}
