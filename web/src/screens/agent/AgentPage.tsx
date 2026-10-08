import { createContext, useCallback, useContext, useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react';
import type { ReactNode } from 'react';
import { flushSync } from 'react-dom';
import { useNavigate } from 'react-router';
import { ApiError, apiGet, apiPost } from '../../api/client';
import type { AgentDetail, NewChatResponse } from '../../api/types.gen';
import { useFrameChrome } from '../../app/chrome';
import { runStateView } from '../../app/runState';
import { AgentAvatar, AgentState, Badge, Banner, Button, ContextGauge, Dialog, DialogClose, Icon, Menu, Tabs, Text, BREAKPOINT_TABBAR, mediaQuery } from '../../ds';
import type { TabSpec } from '../../ds';
import { agentIdentity } from '../../lib/agentIdentity';
import { cx } from '../../lib/cx';
import { useMediaQuery } from '../../lib/useMediaQuery';
import { flattenTree } from '../../state/org';
import { useOrg } from '../../state/OrgProvider';
import '../../styles/chat.css';

export type AgentTab = 'chat' | 'background' | 'about' | 'chats';

const TAB_PATH: Record<AgentTab, string> = { chat: '', background: '/background', about: '/about', chats: '/chats' };

export interface AgentPageContextValue {
  slug: string;
  /** The agent as GET /api/v1/agents/{slug} describes it; null until it loads. */
  detail: AgentDetail | null;
  /** The name to use before the detail loads: the tree's, else the slug. */
  name: string;
  /** Archived agents are read only. */
  archived: boolean;
  /** The chat's live context fill, which the header shows once the chat reports one. */
  setLiveFill(pct: number | null): void;
  /** Bumped when a new chat was started from the header, so the chat can follow the rotation. */
  newChatSeq: number;
  /** Reads the agent again: a new chat has started, so there is one more past chat to count. */
  refreshDetail(): void;
}

const AgentPageContext = createContext<AgentPageContextValue | null>(null);

/** The agent the page is about. Tab bodies call it; outside AgentPage it throws. */
export function useAgentPage(): AgentPageContextValue {
  const ctx = useContext(AgentPageContext);
  if (!ctx) throw new Error('useAgentPage must be used inside <AgentPage>');
  return ctx;
}

function asApiError(err: unknown): ApiError {
  return err instanceof ApiError ? err : new ApiError(0, 'Something went wrong in Kivali.', 'Reload the page. If it keeps happening, whoever runs this Kivali server can look into it.');
}

/** The window's IntersectionObserver, read when the page mounts so a test can stub the global with a fake. */
function observerCtor(): typeof IntersectionObserver | undefined {
  return typeof window !== 'undefined' && typeof window.IntersectionObserver === 'function' ? window.IntersectionObserver : undefined;
}

/**
 * Whether the reader has scrolled past the top of the agent's body. A zero-height sentinel sits at the top of the
 * tab body, below the header; one IntersectionObserver reports when it leaves the top of the viewport (no scroll
 * listener, no timers). The header only ever changes height above the sentinel, so the switch measures the
 * sentinel before and after it commits and scrolls by the difference: what the reader was looking at stays put,
 * and the sentinel stays on the side of the edge it crossed (no flip-flop). At the bottom of the page the browser
 * already clamps the scroll, the sentinel does not move, and nothing is scrolled; at the very top nothing is
 * scrolled either, so a jump to the top shows the top.
 */
function useScrolledPast(): [boolean, (el: HTMLDivElement | null) => void] {
  const [sentinel, setSentinel] = useState<HTMLDivElement | null>(null);
  const [past, setPast] = useState(false);
  useEffect(() => {
    const Observer = observerCtor();
    if (!sentinel || !Observer) return;
    const io = new Observer((entries) => {
      const entry = entries[entries.length - 1];
      if (!entry) return;
      const next = !entry.isIntersecting && entry.boundingClientRect.top < 0;
      // Back at the very top (a jump there) the reader wants the top of the page, not the old position.
      const toTop = !next && window.scrollY <= 0;
      const before = sentinel.getBoundingClientRect().top;
      flushSync(() => setPast(next));
      const after = sentinel.getBoundingClientRect().top;
      if (after !== before && !toTop) window.scrollBy(0, after - before);
    });
    io.observe(sentinel);
    return () => io.disconnect();
  }, [sentinel]);
  return [past, setSentinel];
}

function tokensK(n: number): string {
  return n >= 1000 ? Math.round(n / 1000) + 'k' : String(n);
}

export interface AgentPageProps {
  slug: string;
  tab: AgentTab;
  children?: ReactNode;
}

/**
 * The layout every agent tab shares: the header (avatar, name, role line, state, context, New chat, the ⋯
 * menu) and the tabs Chat · Background · About · Past chats. The tab bar hides and Team is the way back.
 * Header and tabs are pinned to the top of the column, so the tabs are always in reach however long the chat;
 * below 960 the header condenses to one slim row once the body is scrolled.
 */
export function AgentPage({ slug, tab, children }: AgentPageProps) {
  const navigate = useNavigate();
  const { org } = useOrg();
  const node = useMemo(() => flattenTree(org.tree).find((n) => n.slug === slug), [org.tree, slug]);
  const [detail, setDetail] = useState<AgentDetail | null>(null);
  const [loadError, setLoadError] = useState<ApiError | null>(null);
  const [liveFill, setLiveFillState] = useState<number | null>(null);
  const [dialogOpen, setDialogOpen] = useState(false);
  const [starting, setStarting] = useState(false);
  const [newChatError, setNewChatError] = useState<ApiError | null>(null);
  const [newChatSeq, setNewChatSeq] = useState(0);
  const [detailSeq, setDetailSeq] = useState(0);
  const [copied, setCopied] = useState(false);
  // The phone header carries the page's h1 below 960; the name is the h1 only from 960, a paragraph below.
  const desktop = useMediaQuery(mediaQuery(BREAKPOINT_TABBAR));
  // The header and tabs stay pinned at the top of the column. Below 960 the header condenses to one slim row once
  // the reader has scrolled past the top of the body, so the transcript keeps most of the screen.
  const [scrolledPast, sentinelRef] = useScrolledPast();
  const condensed = !desktop && scrolledPast;
  const sectionRef = useRef<HTMLElement>(null);
  const headRef = useRef<HTMLElement>(null);

  // The tabs pin directly under the header, whose height depends on its contents and state: measure it.
  useLayoutEffect(() => {
    const section = sectionRef.current;
    const head = headRef.current;
    if (!section || !head) return;
    const apply = () => section.style.setProperty('--app-agent-head-h', String(head.offsetHeight) + 'px');
    apply();
    if (typeof ResizeObserver !== 'function') return;
    const ro = new ResizeObserver(apply);
    ro.observe(head);
    return () => ro.disconnect();
  }, []);

  useEffect(() => {
    setDetail(null);
    setLoadError(null);
    setLiveFillState(null);
  }, [slug]);

  // Read again on refreshDetail, keeping what is shown until the answer is in.
  useEffect(() => {
    const abort = new AbortController();
    apiGet<AgentDetail>('/api/v1/agents/' + encodeURIComponent(slug), { signal: abort.signal })
      .then(setDetail)
      .catch((err: unknown) => {
        if (err instanceof DOMException && err.name === 'AbortError') return;
        setLoadError(asApiError(err));
      });
    return () => abort.abort();
  }, [slug, detailSeq]);

  const name = detail?.name || node?.name || slug;
  const identity = agentIdentity({ slug, name, icon: node?.icon || detail?.icon });
  const archived = detail?.archived ?? false;
  useFrameChrome({ title: name, tabBar: false, back: { to: '/team', label: 'Team' }, width: 'transcript' });

  const setLiveFill = useCallback((pct: number | null) => setLiveFillState(pct), []);
  const refreshDetail = useCallback(() => setDetailSeq((n) => n + 1), []);

  const pct = liveFill ?? node?.contextPct ?? detail?.context.pct ?? 0;
  const ctxTitle = detail && detail.context.limit ? '~' + tokensK(Math.round((detail.context.limit * pct) / 100)) + ' of ~' + tokensK(detail.context.limit) + ' tokens' : undefined;
  // The snapshot says `waiting` both for background tasks still running and for an agent held after Stop
  // (org_state.go agentState); only the count tells them apart. A held agent is not waiting on tasks.
  const view = node ? runStateView(node.state, node.waitingTasks) : null;
  const stateEl = archived ? <Badge>Archived</Badge> : view ? <AgentState state={view.state} {...(view.label ? { label: view.label } : {})} /> : <AgentState state="idle" />;

  const startNewChat = async () => {
    setStarting(true);
    setNewChatError(null);
    try {
      const res = await apiPost<NewChatResponse>('/api/v1/agents/' + encodeURIComponent(slug) + '/new-chat');
      setDialogOpen(false);
      // Not started: the chat was empty, so there is nothing to fold and nothing to follow.
      if (res.started) setNewChatSeq((n) => n + 1);
      if (tab !== 'chat') void navigate('/agents/' + slug);
    } catch (err) {
      setDialogOpen(false);
      const e = asApiError(err);
      if (e.status === 409) setNewChatError(new ApiError(409, name + ' is in the middle of a turn, so a new chat cannot start yet.', 'Try again when it finishes, or press Stop first.'));
      else if (e.status === 503) setNewChatError(new ApiError(503, 'No model is connected yet.', e.who));
      else setNewChatError(e);
    } finally {
      setStarting(false);
    }
  };

  const copyLink = () => {
    const url = window.location.origin + window.location.pathname;
    void navigator.clipboard?.writeText(url).then(
      () => setCopied(true),
      () => setCopied(false),
    );
  };

  // The gauge's threshold: past it the context reading is the view's one signal highlight and New chat is primary.
  const long = Math.round(pct) >= 80;
  const menu = (
    <div className="app-agent-menu">
      <Menu
        trigger={<Button variant="ghost" iconOnly icon={<Icon name="ellipsis" />} aria-label="More" />}
        items={[{ label: copied ? 'Link copied' : 'Copy link', icon: 'link', onSelect: copyLink }]}
      />
    </div>
  );

  const ctx = useMemo<AgentPageContextValue>(
    () => ({ slug, detail, name, archived, setLiveFill, newChatSeq, refreshDetail }),
    [slug, detail, name, archived, setLiveFill, newChatSeq, refreshDetail],
  );

  // The route decides which tab is current, so only that tab has a body; Radix renders it as the tab's real
  // panel (role tabpanel, labelled by its trigger, the id the trigger's aria-controls names).
  const body = (
    <div className="app-agent-body">
      <div ref={sentinelRef} className="app-agent-sentinel" aria-hidden="true" />
      {children}
    </div>
  );
  // The snapshot counts the agent's tasks still out, live; the detail's count is the same number as of its one
  // read, and stands in for an agent the tree does not hold.
  const backgroundCount = node ? node.waitingTasks : detail?.counts.background;
  const tabs: TabSpec[] = [
    { value: 'chat', label: 'Chat' },
    backgroundCount !== undefined ? { value: 'background', label: 'Background', count: backgroundCount } : { value: 'background', label: 'Background' },
    { value: 'about', label: 'About' },
    detail ? { value: 'chats', label: 'Past chats', count: detail.counts.past_chats } : { value: 'chats', label: 'Past chats' },
  ].map((t) => (t.value === tab ? { ...t, content: body } : t));

  return (
    <AgentPageContext.Provider value={ctx}>
      <section ref={sectionRef} className="app-agent" aria-label={name}>
        <header ref={headRef} className={cx('app-agent-head', 'is-pinned', condensed && 'is-condensed')}>
          {condensed ? (
            // Below 960, scrolled: one slim row. The phone header (and its back link) has scrolled away, so the
            // row carries the way back; the stylesheet hides the phone header's own back link meanwhile.
            <>
              <Button variant="ghost" iconOnly icon={<Icon name="chevron-left" />} aria-label="Back to Team" onClick={() => void navigate('/team')} />
              <AgentAvatar {...identity} size={24} />
              <Text as="p" variant="body" className="app-agent-name">
                {name}
              </Text>
              {archived ? <Badge>Archived</Badge> : view ? <AgentState compact state={view.state} {...(view.label ? { label: view.label } : {})} /> : <AgentState compact state="idle" />}
              {!archived && (
                <>
                  <span className="app-agent-ctx" {...(ctxTitle ? { title: ctxTitle } : {})}>
                    <Badge {...(long ? { tone: 'signal' as const, variant: 'solid' as const } : {})} mono>
                      <span className="app-sr-only">Context </span>
                      {Math.round(pct) + '%'}
                      <span className="app-sr-only"> full</span>
                    </Badge>
                  </span>
                  <Button variant={long ? 'primary' : 'ghost'} iconOnly icon={<Icon name="rotate-ccw" />} aria-label="New chat" onClick={() => setDialogOpen(true)} />
                </>
              )}
              {menu}
            </>
          ) : (
            <>
              <AgentAvatar {...identity} size={40} />
              <div className="app-agent-titles">
                <Text as={desktop ? 'h1' : 'p'} variant="heading" className="app-agent-name">
                  {name}
                </Text>
                {detail?.role_line && (
                  <Text as="p" variant="caption" tone="muted" className="app-agent-role">
                    {detail.role_line}
                  </Text>
                )}
              </div>
              {menu}
              <div className="app-agent-status">
                {stateEl}
                {!archived && (
                  <ContextGauge value={pct} onNewChat={() => setDialogOpen(true)} {...(ctxTitle ? { title: ctxTitle } : {})} />
                )}
              </div>
            </>
          )}
        </header>
        {loadError && (
          <Banner tone="danger" title={loadError.message}>
            {loadError.who}
          </Banner>
        )}
        {newChatError && (
          <Banner tone="warning" title={newChatError.message} onDismiss={() => setNewChatError(null)}>
            {newChatError.who}
          </Banner>
        )}
        <Tabs
          className="app-agent-tabs"
          tabs={tabs}
          value={tab}
          onValueChange={(v) => {
            const next = v as AgentTab;
            if (next !== tab) void navigate('/agents/' + slug + TAB_PATH[next]);
          }}
        />
        <Dialog
          open={dialogOpen}
          onOpenChange={setDialogOpen}
          title={'Start a new chat with ' + name + '?'}
          description={
            name +
            ' first folds this chat into what it remembers and its habits. Then this chat moves to past chats and a fresh one starts. Its assignments and background work carry on.'
          }
          footer={
            <>
              <DialogClose asChild>
                <Button variant="secondary">Cancel</Button>
              </DialogClose>
              <Button variant="primary" loading={starting} onClick={() => void startNewChat()}>
                Start new chat
              </Button>
            </>
          }
        />
      </section>
    </AgentPageContext.Provider>
  );
}
