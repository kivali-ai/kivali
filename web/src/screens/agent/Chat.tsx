import { useCallback, useEffect, useLayoutEffect, useMemo, useReducer, useRef, useState } from 'react';
import { ApiError, apiDelete, apiGet, apiPost, apiPostForm } from '../../api/client';
import type { Chat as ChatData, ChatSettings, MessagePostResponse, PendingRestoreResponse, StopResponse } from '../../api/types.gen';
import { Badge, Banner, Button, Composer, EmptyState, Icon, Select, Skeleton, Toast } from '../../ds';
import type { ToastTone } from '../../ds';
import { pickFiles } from '../../lib/pickFiles';
import { flattenTree } from '../../state/org';
import { useOrg } from '../../state/OrgProvider';
import { THINKING_PROMOTE_MS, canStop, currentEfforts, initialTranscript, reconcileSnapshot, reduceTranscript, selectedEffort, thinkingView } from '../../state/transcript';
import type { PendingView, TranscriptState } from '../../state/transcript';
import { useAgentPage } from './AgentPage';
import { useChatDeps } from './chatDeps';
import { clearDraft, loadDraft, saveDraft } from './drafts';
import { Transcript } from './Transcript';
import { useAgentStream } from './useAgentStream';

/** An undo toast stays as long as the server keeps a deleted message (about 10 s). */
export const UNDO_TOAST_MS = 10_000;
const TOAST_MS = 5_000;
/** How close to the bottom still counts as reading the bottom. */
const BOTTOM_SLOP = 60;

interface ToastEntry {
  id: number;
  tone: ToastTone;
  title: string;
  body?: string;
  /** The pending message Undo restores. */
  undoId?: string;
}

function asApiError(err: unknown): ApiError {
  return err instanceof ApiError ? err : new ApiError(0, 'Something went wrong in Kivali.', 'Reload the page. If it keeps happening, whoever runs this Kivali server can look into it.');
}

function scroller(): HTMLElement {
  return document.scrollingElement instanceof HTMLElement ? document.scrollingElement : document.documentElement;
}

function fileKey(f: File): string {
  return f.name + '\u0000' + f.size + '\u0000' + f.lastModified;
}

function ChatToast({ entry, onDone, onUndo }: { entry: ToastEntry; onDone(id: number): void; onUndo(entry: ToastEntry): void }) {
  const { id, undoId } = entry;
  useEffect(() => {
    const t = setTimeout(() => onDone(id), undoId ? UNDO_TOAST_MS : TOAST_MS);
    return () => clearTimeout(t);
  }, [id, undoId, onDone]);
  return (
    <Toast
      tone={entry.tone}
      title={entry.title}
      onDismiss={() => onDone(id)}
      {...(undoId
        ? {
            action: (
              <Button variant="ghost" size="sm" onClick={() => onUndo(entry)}>
                Undo
              </Button>
            ),
          }
        : {})}
    >
      {entry.body}
    </Toast>
  );
}

/**
 * The Chat tab: the transcript anchored to the bottom, and the composer with the model and effort pickers
 * and Stop. It follows the agent's stream while a turn runs and the org snapshot the rest of the time.
 * An archived agent's chat is read only.
 */
export function Chat() {
  const { slug, name, archived: agentArchived, setLiveFill, newChatSeq, refreshDetail } = useAgentPage();
  const deps = useChatDeps();
  const { org } = useOrg();
  const [state, dispatch] = useReducer(reduceTranscript, { slug, name }, initialTranscript);
  const stateRef = useRef<TranscriptState>(state);
  stateRef.current = state;
  const stream = useAgentStream(slug, dispatch, deps);
  const base = '/api/v1/agents/' + encodeURIComponent(slug);

  const [loadError, setLoadError] = useState<ApiError | null>(null);
  const [actionError, setActionError] = useState<ApiError | null>(null);
  const [draft, setDraft] = useState(() => loadDraft(slug, deps.now()));
  const [files, setFiles] = useState<File[]>([]);
  const [posting, setPosting] = useState(false);
  const [stopping, setStopping] = useState(false);
  const [toasts, setToasts] = useState<ToastEntry[]>([]);
  const toastSeq = useRef(0);
  const [now, setNow] = useState(() => deps.now());

  const readOnly = agentArchived || state.archived;

  // ---- Loading ----
  const loadSeq = useRef(0);
  const load = useCallback(async (): Promise<ChatData | null> => {
    const seq = ++loadSeq.current;
    try {
      const chat = await apiGet<ChatData>(base + '/chat');
      if (seq !== loadSeq.current) return null;
      dispatch({ type: 'loaded', chat, at: deps.now() });
      setNow(deps.now());
      setLoadError(null);
      return chat;
    } catch (err) {
      if (seq === loadSeq.current) setLoadError(asApiError(err));
      return null;
    }
  }, [base, deps]);

  // Every load that says a turn is running follows it on the stream, so `running` always has a stream to
  // settle it: a live turn streams (and ends with `done`), and nothing running answers 204, which clears it.
  // The fetch after `done` can still say running (the server clears its in-flight mark just after it emits
  // `done`); reopening is what keeps Stop from staying up on that stale answer. A no-op when one is open.
  const loadAndFollow = useCallback(
    async (isCancelled: () => boolean = () => false): Promise<void> => {
      const chat = await load();
      if (!isCancelled() && chat && chat.running && !chat.archived) stream.open();
    },
    [load, stream],
  );

  useEffect(() => {
    let cancelled = false;
    void loadAndFollow(() => cancelled);
    return () => {
      cancelled = true;
    };
  }, [loadAndFollow]);

  // The reducer asks for a refetch: a turn ended, a rotation finished, a send was never painted.
  useEffect(() => {
    if (state.reloadSeq > 0) void loadAndFollow();
  }, [state.reloadSeq, loadAndFollow]);

  // A new chat started from the header: say so at once, and follow the turn that folds this chat. Only a
  // request made while this tab is showing counts: the header outlives the tab, so a chat that mounts
  // later (New chat from another tab, or coming back to this one) learns where things stand from the GET.
  const followedNewChat = useRef(newChatSeq);
  useEffect(() => {
    if (newChatSeq === followedNewChat.current) return;
    followedNewChat.current = newChatSeq;
    dispatch({ type: 'rotation_requested', at: deps.now() });
    stream.open();
  }, [newChatSeq, stream, deps]);

  // The fresh chat took this one's place: there is one more past chat for the header to count.
  useEffect(() => {
    if (state.rotationSeq > 0) refreshDetail();
  }, [state.rotationSeq, refreshDetail]);

  // The header's context reading follows the chat's live fill.
  const fillPct = state.fill && state.loaded ? state.fill.pct : null;
  useEffect(() => {
    setLiveFill(fillPct);
  }, [fillPct, setLiveFill]);
  useEffect(() => () => setLiveFill(null), [setLiveFill]);

  // ---- Following the org snapshot ----
  const resume = useRef({ armed: false, midTurn: false });
  useEffect(() => {
    const onVisibility = () => {
      if (document.visibilityState === 'visible') {
        resume.current.armed = true;
      } else {
        const s = stateRef.current;
        resume.current.midTurn = s.running || thinkingView(s) !== null;
      }
    };
    document.addEventListener('visibilitychange', onVisibility);
    return () => document.removeEventListener('visibilitychange', onVisibility);
  }, []);

  // This agent in the snapshot: undefined before the first snapshot, null when it is not listed.
  const node = useMemo(() => {
    if (!org.ready) return undefined;
    return flattenTree(org.tree).find((n) => n.slug === slug) ?? null;
  }, [org.ready, org.tree, slug]);
  const agentState = node === undefined ? undefined : (node?.state ?? null);

  useEffect(() => {
    const s = stateRef.current;
    if (agentState === undefined || !s.loaded || readOnly) return;
    const r = resume.current;
    const decision = reconcileSnapshot({
      agentState,
      snapshotWaiting: node?.waitingTasks ?? 0,
      streamOpen: stream.isOpen(),
      resumed: r.armed,
      midTurnAtHide: r.midTurn,
      running: s.running,
      waitingTasks: s.waitingTasks,
    });
    if (r.armed) resume.current = { armed: false, midTurn: false };
    if (decision.waiting !== undefined) dispatch({ type: 'waiting', tasks: decision.waiting });
    if (decision.open) stream.open(decision.open === 'force');
    if (decision.refetch) void loadAndFollow();
    // Runs once per snapshot: org.tree is a new array on each one.
  }, [agentState, node, org.tree, readOnly, stream, loadAndFollow, state.loaded]);

  // ---- Thinking: the step debounce and the elapsed clock ----
  const { pendingStep, pendingSummary } = state.thinking;
  useEffect(() => {
    if (!pendingStep && !pendingSummary) return;
    const t = setTimeout(() => dispatch({ type: 'thinking_promote' }), THINKING_PROMOTE_MS);
    return () => clearTimeout(t);
  }, [pendingStep, pendingSummary]);

  const view = thinkingView(state);
  const ticking = view !== null && view.since > 0;
  useEffect(() => {
    if (!ticking) return;
    setNow(deps.now());
    const t = setInterval(() => setNow(deps.now()), 1000);
    return () => clearInterval(t);
  }, [ticking, deps]);

  // ---- Anchored to the bottom ----
  useEffect(() => {
    const onScroll = () => {
      const el = scroller();
      const atBottom = el.scrollHeight - el.scrollTop - el.clientHeight <= BOTTOM_SLOP;
      if (atBottom !== stateRef.current.atBottom) dispatch({ type: 'scrolled', atBottom });
    };
    window.addEventListener('scroll', onScroll, { passive: true });
    return () => window.removeEventListener('scroll', onScroll);
  }, []);

  const toBottom = useCallback(() => {
    const el = scroller();
    el.scrollTop = el.scrollHeight;
  }, []);

  // Stick to the bottom while the person is reading it; leave them where they are otherwise.
  useLayoutEffect(() => {
    if (state.atBottom) toBottom();
  }, [state.rows, state.pending, view?.label, state.atBottom, toBottom]);

  const jumpToBottom = () => {
    toBottom();
    dispatch({ type: 'scrolled', atBottom: true });
  };

  // ---- Toasts ----
  const addToast = useCallback((t: Omit<ToastEntry, 'id'>) => {
    const id = ++toastSeq.current;
    setToasts((list) => [...list, { ...t, id }]);
  }, []);
  const dropToast = useCallback((id: number) => setToasts((list) => list.filter((t) => t.id !== id)), []);

  // ---- Actions ----
  const send = async (text: string) => {
    const body = text.trim();
    // Files alone are a message (chat-attachments: "submitting with only attachments still posts").
    if ((!body && files.length === 0) || posting) return;
    // Always multipart: the handler reads form fields only (text, and one `attachment` per file).
    const form = new FormData();
    form.append('text', body);
    for (const f of files) form.append('attachment', f, f.name);
    setPosting(true);
    setActionError(null);
    dispatch({ type: 'dismiss_error' });
    try {
      const response = await apiPostForm<MessagePostResponse>(base + '/messages', form);
      dispatch({ type: 'send_posted', text: body, response, at: deps.now() });
      setDraft('');
      clearDraft(slug);
      setFiles([]);
      jumpToBottom();
      // The agent is busy with this now: its stream carries the message and the reply.
      stream.open();
    } catch (err) {
      setActionError(asApiError(err));
    } finally {
      setPosting(false);
    }
  };

  const attach = async () => {
    const picked = await pickFiles({ multiple: true });
    if (picked.length === 0) return;
    setFiles((list) => {
      const seen = new Set(list.map(fileKey));
      const next = [...list];
      for (const f of picked) {
        if (seen.has(fileKey(f))) continue;
        seen.add(fileKey(f));
        next.push(f);
      }
      return next;
    });
  };

  const stop = async () => {
    setStopping(true);
    try {
      await apiPost<StopResponse>(base + '/stop');
    } catch (err) {
      setActionError(asApiError(err));
    } finally {
      setStopping(false);
    }
  };

  const changeSetting = async (kind: 'model' | 'effort', value: string) => {
    try {
      const res = await apiPost<ChatSettings>(base + '/' + kind, kind === 'model' ? { model: value } : { effort: value });
      dispatch({ type: 'settings', currentModel: res.current_model, currentEffort: res.current_effort });
    } catch (err) {
      setActionError(asApiError(err));
    }
  };

  const deletePending = async (p: PendingView) => {
    dispatch({ type: 'pending_busy', id: p.id, op: 'deleting' });
    try {
      await apiDelete(base + '/pending/' + encodeURIComponent(p.id));
      dispatch({ type: 'pending_removed', id: p.id });
      addToast({ tone: 'info', title: 'Message deleted', body: 'It was never delivered to ' + name + '.', undoId: p.id });
    } catch (err) {
      dispatch({ type: 'pending_busy', id: p.id, op: null });
      const e = asApiError(err);
      if (e.status === 409) {
        dispatch({ type: 'event', name: 'pending_offered', data: { id: p.id }, at: deps.now() });
        addToast({ tone: 'info', title: 'Already with ' + name, body: 'It reads the message at its next step. Send now delivers it at once.' });
      } else if (e.status !== 404) {
        setActionError(e);
      }
    }
  };

  const restore = async (entry: ToastEntry) => {
    dropToast(entry.id);
    if (!entry.undoId) return;
    try {
      const message = await apiPost<PendingRestoreResponse>(base + '/pending/' + encodeURIComponent(entry.undoId) + '/restore');
      dispatch({ type: 'pending_restored', message });
      if (message.delivered_ts) stream.open();
    } catch (err) {
      const e = asApiError(err);
      if (e.status === 410) addToast({ tone: 'info', title: 'Too late to restore', body: 'The message was deleted more than a few seconds ago.' });
      else setActionError(e);
    }
  };

  const sendNow = async (p: PendingView) => {
    dispatch({ type: 'pending_busy', id: p.id, op: 'sending' });
    try {
      await apiPost(base + '/pending/' + encodeURIComponent(p.id) + '/send-now');
      stream.open();
    } catch (err) {
      dispatch({ type: 'pending_busy', id: p.id, op: null });
      const e = asApiError(err);
      // 404: it was delivered or deleted meanwhile, and the stream already said so.
      if (e.status !== 404) setActionError(e);
    }
  };

  const onDraft = (v: string) => {
    setDraft(v);
    saveDraft(slug, v, deps.now());
  };

  // ---- Render ----
  const settings = state.settings;
  const modelOptions = useMemo(() => {
    const opts = settings.models.map((m) => ({ value: m.id, label: m.legacy ? m.label + ' · pinned' : m.label }));
    if (settings.currentModel && !opts.some((o) => o.value === settings.currentModel)) opts.push({ value: settings.currentModel, label: settings.currentModel });
    return opts;
  }, [settings.models, settings.currentModel]);
  // The effort chip offers what the current model's row offers, and hides when it offers nothing.
  const effortOptions = useMemo(() => currentEfforts(settings).map((e) => ({ value: e.id, label: e.label })), [settings]);
  const effortValue = selectedEffort(settings);
  const agent = useMemo(() => ({ slug, name }), [slug, name]);
  const empty = state.loaded && state.rows.length === 0 && state.pending.length === 0 && !view;

  const footer = (
    <>
      {files.map((f) => (
        <span key={fileKey(f)} className="app-chat-file">
          <Badge variant="outline" mono>
            {f.name}
          </Badge>
          <Button variant="ghost" size="sm" iconOnly icon={<Icon name="x" />} aria-label={'Remove ' + f.name} onClick={() => setFiles((list) => list.filter((x) => x !== f))} />
        </span>
      ))}
      {modelOptions.length > 0 && (
        <span className="app-chat-select">
          <Select aria-label="Model" options={modelOptions} value={settings.currentModel} onChange={(e) => void changeSetting('model', e.target.value)} />
        </span>
      )}
      {effortOptions.length > 0 && (
        <span className="app-chat-select">
          <Select aria-label="Effort" options={effortOptions} value={effortValue} onChange={(e) => void changeSetting('effort', e.target.value)} />
        </span>
      )}
      <span className="app-chat-spacer" />
      {canStop(state) && (
        <Button variant="secondary" size="sm" icon={<Icon name="square" />} loading={stopping} onClick={() => void stop()}>
          Stop
        </Button>
      )}
    </>
  );

  return (
    <div className="app-chat">
      {loadError && (
        <Banner tone="danger" title={loadError.message}>
          {loadError.who}
        </Banner>
      )}
      <div className="app-chat-scroll">
        {!state.loaded && !loadError && (
          <div className="app-chat-skeleton" aria-hidden="true">
            <Skeleton width="60%" />
            <Skeleton width="80%" />
            <Skeleton width="40%" />
          </div>
        )}
        {empty && <EmptyState title="No messages yet">{readOnly ? 'This chat has no messages.' : 'Write to ' + name + ' below.'}</EmptyState>}
        {state.loaded && (
          <Transcript
            rows={state.rows}
            agent={agent}
            pending={state.pending}
            thinking={view}
            now={now}
            readOnly={readOnly}
            onDeletePending={(p) => void deletePending(p)}
            onSendNow={(p) => void sendNow(p)}
          />
        )}
      </div>
      {!state.atBottom && state.unreadBelow > 0 && (
        <div className="app-chat-jump">
          <Button variant="secondary" size="sm" icon={<Icon name="chevron-down" />} onClick={jumpToBottom}>
            New messages
          </Button>
        </div>
      )}
      {!readOnly && (
        <div className="app-chat-compose">
          {state.rotating && (
            <Banner tone="info" title={name + ' is starting a new chat.'}>
              It is folding this chat into what it remembers. Messages you send now wait for the new chat.
            </Banner>
          )}
          {state.error && (
            <Banner tone="danger" title={state.error} onDismiss={() => dispatch({ type: 'dismiss_error' })}>
              {'The turn stopped. Send ' + name + ' another message to carry on.'}
            </Banner>
          )}
          {actionError && (
            <Banner tone="danger" title={actionError.message} onDismiss={() => setActionError(null)}>
              {actionError.who}
            </Banner>
          )}
          <Composer
            placeholder={'Message ' + name}
            value={draft}
            onChange={onDraft}
            onSend={(v) => void send(v)}
            busy={posting}
            onAttach={() => void attach()}
            allowEmpty={files.length > 0}
            footer={footer}
          />
        </div>
      )}
      {toasts.length > 0 && (
        <div className="app-chat-toasts">
          {toasts.map((t) => (
            <ChatToast key={t.id} entry={t} onDone={dropToast} onUndo={(e) => void restore(e)} />
          ))}
        </div>
      )}
    </div>
  );
}
