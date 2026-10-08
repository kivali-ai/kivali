import { useCallback, useEffect, useMemo, useState } from 'react';
import { Link, useHref, useNavigate, useParams } from 'react-router';
import { ApiError, apiGet, apiPostForm } from '../../api/client';
import type { AttachmentRef, NeedActionResponse, PersonRef, Proposal as ProposalData, ProposalDoc, ProposalMove, ProposalSummary } from '../../api/types.gen';
import { useFrameChrome } from '../../app/chrome';
import { AgentAvatar, Badge, Banner, Button, Card, DocDiff, EmptyState, Icon, Prose, Skeleton, Text, TextField, BREAKPOINT_TABBAR, mediaQuery } from '../../ds';
import { agentIdentity, identityColorFor } from '../../lib/agentIdentity';
import { renderMarkdown } from '../../lib/markdown';
import { pickFiles } from '../../lib/pickFiles';
import { relativeTime } from '../../lib/time';
import { useMediaQuery } from '../../lib/useMediaQuery';
import { routerPath } from '../../state/home';
import '../../styles/proposals.css';

const KIND_LABEL: Record<string, string> = {
  hire: 'Hire',
  role_update: 'Role update',
  handbook_update: 'Handbook update',
  offboard: 'Offboard',
  reorg: 'Reorg',
};

const DOC_ICON: Record<string, string> = {
  role: 'file-text',
  memory: 'brain',
  handbook: 'scroll-text',
};

const NOTE_PLACEHOLDER: Record<string, string> = {
  hire: 'Go ahead, and keep me posted.',
  role_update: 'Agreed. Tell me if anything changes.',
  handbook_update: 'Yes. Tell every agent when it lands.',
  offboard: 'Fine. Bring it back later if we need it.',
  reorg: 'OK. Keep me posted on how it goes.',
};

function asApiError(err: unknown): ApiError {
  return err instanceof ApiError ? err : new ApiError(0, 'Something went wrong in Kivali.', 'Reload the page. If it keeps happening, whoever runs this Kivali server can look into it.');
}

function formatSize(bytes: number): string {
  if (bytes <= 0) return '';
  if (bytes < 1024) return bytes + ' B';
  if (bytes < 1024 * 1024) return Math.round(bytes / 1024) + ' KB';
  return (bytes / (1024 * 1024)).toFixed(1) + ' MB';
}

/** The request path the server keys a proposal by, one URL-encoded segment at a time. */
function proposalUrl(path: string): string {
  return '/api/v1/proposals/' + path.split('/').map(encodeURIComponent).join('/');
}

/** "3h ago", "2d ago"; a day name or a date reads on its own ("Yesterday", "26 Sept"). */
function agoLine(rel: string): string {
  if (rel === 'now') return 'just now';
  return /^\d+[mhd]$/.test(rel) ? rel + ' ago' : rel;
}

/** The summary card's title: the agent it is about, the moves of a reorg, or the document everyone reads. */
function summaryTitle(p: ProposalData): string | undefined {
  const { agent, moves } = p.summary;
  if (agent) return agent.name;
  if (moves.length > 0) return moves.length === 1 ? '1 move' : moves.length + ' moves';
  if (p.kind === 'handbook_update') return 'Handbook';
  return undefined;
}

/** The summary card's muted line, from what the wire says. */
function summaryMeta(p: ProposalData): string | undefined {
  const agent = p.summary.agent;
  if (p.kind === 'hire') return 'New agent';
  if (p.kind === 'handbook_update') return 'Applies to every agent';
  if (agent?.role_title && agent.role_title !== agent.name) return agent.role_title;
  return undefined;
}

/** The button beside a resolution: where the approval led. Only an approval carries a link. */
export function linkLabel(kind: string, summary: ProposalSummary): string {
  const name = summary.agent?.name;
  switch (kind) {
    case 'hire':
    case 'role_update':
      return name ? 'Open ' + name : 'Open agent';
    case 'handbook_update':
      return 'Read it';
    case 'offboard':
      return 'Open the archive';
    case 'reorg':
      return 'Open Team';
    default:
      return 'Open';
  }
}

function approveLabel(p: ProposalData): string {
  switch (p.kind) {
    case 'hire':
      return 'Approve hire';
    case 'role_update':
      return 'Approve role update';
    case 'handbook_update':
      return 'Approve update';
    case 'offboard':
      return 'Offboard ' + (p.summary.agent?.name ?? 'agent');
    case 'reorg':
      return 'Approve reorg';
    default:
      return 'Approve';
  }
}

interface Outcome {
  approved: boolean;
  result: string;
  link?: string;
}

function Facts({ facts, reportsTo }: { facts: ProposalSummary['facts']; reportsTo?: PersonRef | undefined }) {
  if (facts.length === 0) return null;
  return (
    <dl className="app-proposal-facts">
      {facts.map((f) => (
        <div key={f.label} className="app-proposal-fact">
          <Text as="dt" variant="label" tone="muted">
            {f.label}
          </Text>
          <Text as="dd" variant="body" className="app-proposal-fact-value">
            {reportsTo && f.label === 'Reports to' && f.value === reportsTo.name &&<AgentAvatar name={reportsTo.name} size={20} color={identityColorFor(reportsTo.slug)} />}
            {f.value}
          </Text>
        </div>
      ))}
    </dl>
  );
}

function Move({ move }: { move: ProposalMove }) {
  return (
    <li className="app-proposal-move">
      <span className="app-proposal-move-who">
        <AgentAvatar name={move.name} size={24} color={identityColorFor(move.slug)} />
        <Text variant="body">{move.name}</Text>
      </span>
      <span className="app-proposal-move-line">
        {move.from && (
          <>
            <AgentAvatar name={move.from.name} size={20} color={identityColorFor(move.from.slug)} />
            <Text tone="muted" className="app-proposal-struck">
              {move.from.name}
            </Text>
            <Icon name="arrow-right" />
          </>
        )}
        <AgentAvatar name={move.to.name} size={20} color={identityColorFor(move.to.slug)} />
        <Text>{move.to.name}</Text>
      </span>
      {move.failed && (
        <span className="app-proposal-move-failed">
          <Icon name="triangle-alert" />
          <Text variant="caption" tone="danger">
            Not made: {move.failed}
          </Text>
        </span>
      )}
    </li>
  );
}

/**
 * The summary card, as the canvas draws it: the Card title names the agent (or the moves, or the handbook),
 * the tile is the 56 avatar beside the facts. Card titles are h3, so an unseen h2 keeps the outline in order.
 */
function Summary({ p }: { p: ProposalData }) {
  const { agent, moves, facts } = p.summary;
  if (!agent && moves.length === 0 && facts.length === 0) return null;
  return (
    <section className="app-proposal-summary-block" aria-labelledby="app-proposal-summary-h">
      <Text as="h2" id="app-proposal-summary-h" className="app-sr-only">
        Summary
      </Text>
      <Card title={summaryTitle(p)} meta={summaryMeta(p)}>
        <div className="app-proposal-summary">
          {agent && (
            <div className="app-proposal-agent">
              <AgentAvatar {...agentIdentity(agent)} size={56} />
              <Facts facts={facts} reportsTo={agent.reports_to} />
            </div>
          )}
          {moves.length > 0 && (
            <ul className="app-proposal-moves" aria-label="Reporting line changes">
              {moves.map((m) => (
                <Move key={m.slug + '>' + m.to.slug} move={m} />
              ))}
            </ul>
          )}
          {!agent && <Facts facts={facts} />}
        </div>
      </Card>
    </section>
  );
}

function DocBlock({ doc }: { doc: ProposalDoc }) {
  return (
    <section className="app-proposal-doc" aria-label={doc.title}>
      <div className="app-proposal-doc-head">
        <Icon name={DOC_ICON[doc.key] ?? 'file-text'} size={20} />
        <Text as="h2" variant="heading" tone="default">
          {doc.title}
        </Text>
        <Text variant="label" tone="muted">
          {doc.meta}
        </Text>
      </div>
      <DocDiff before={doc.before ?? null} after={doc.after} />
    </section>
  );
}

function Attached({ items }: { items: readonly AttachmentRef[] }) {
  if (items.length === 0) return null;
  return (
    <ul className="app-proposal-attachments" aria-label="Attachments">
      {items.map((a) => {
        const size = formatSize(a.size_bytes);
        return (
          <li key={a.url + a.name}>
            <Button size="sm" variant="secondary" icon={<Icon name="download" />} title={'Download ' + a.name} onClick={() => window.open(a.url, '_blank', 'noopener')}>
              {size ? a.name + ' · ' + size : a.name}
            </Button>
          </li>
        );
      })}
    </ul>
  );
}

export interface ProposalProps {
  /** The time relative stamps read, in ms since the epoch. Tests pass a fixed one. */
  now?: number;
}

/** A proposal review page: what is asked, what changes, and the decision. Home stays active; the frame supplies the way back. */
export function Proposal({ now }: ProposalProps) {
  const splat = useParams()['*'] ?? '';
  const navigate = useNavigate();
  const base = useHref('/').replace(/\/$/, '');
  const [data, setData] = useState<ProposalData | null>(null);
  const [loadError, setLoadError] = useState<ApiError | null>(null);
  const [outcome, setOutcome] = useState<Outcome | null>(null);
  const [actionError, setActionError] = useState<ApiError | null>(null);
  const [busy, setBusy] = useState<'approve' | 'deny' | null>(null);
  const [message, setMessage] = useState('');
  const [files, setFiles] = useState<File[]>([]);
  const [attempt, setAttempt] = useState(0);
  const [clockNow] = useState(() => Date.now());

  useFrameChrome({ title: data?.title ?? 'Proposal', tabBar: false, width: 'transcript', back: { to: '/', label: 'Home' } });
  // The phone header carries the page's h1 below 960; the title is the h1 only from 960, a paragraph below.
  const desktop = useMediaQuery(mediaQuery(BREAKPOINT_TABBAR));

  useEffect(() => {
    const ctl = new AbortController();
    setData(null);
    setLoadError(null);
    setOutcome(null);
    setActionError(null);
    apiGet<ProposalData>(proposalUrl(splat), { signal: ctl.signal }).then(
      (p) => setData(p),
      (err: unknown) => {
        if (ctl.signal.aborted) return;
        setLoadError(asApiError(err));
      },
    );
    return () => ctl.abort();
  }, [splat, attempt]);

  const attach = useCallback(async () => {
    const picked = await pickFiles();
    if (picked.length > 0) setFiles((prev) => [...prev, ...picked]);
  }, []);

  const answer = useCallback(
    async (verb: 'approve' | 'deny') => {
      if (!data || busy) return;
      const form = new FormData();
      form.append('path', data.path);
      form.append('message', message);
      for (const f of files) form.append('attachments[]', f);
      setBusy(verb);
      setActionError(null);
      try {
        const res = await apiPostForm<NeedActionResponse>('/api/v1/needs/' + verb, form);
        const approved = verb === 'approve';
        const next: Outcome = { approved, result: res.result || (approved ? 'Approved.' : 'Denied.') };
        if (res.link) next.link = res.link;
        setOutcome(next);
      } catch (err) {
        const e = asApiError(err);
        // Already answered elsewhere: show how it was resolved instead of the form.
        if (e.status === 404) setAttempt((n) => n + 1);
        else setActionError(e);
      } finally {
        setBusy(null);
      }
    },
    [data, busy, message, files],
  );

  const reasonHtml = useMemo(() => (data ? renderMarkdown(data.reason_md) : ''), [data]);

  if (loadError) {
    if (loadError.status === 404) {
      return (
        <EmptyState title="This proposal isn't here" action={<Link to="/">Back to Home</Link>}>
          It may have been withdrawn, or the link is wrong. Home lists what needs you.
        </EmptyState>
      );
    }
    return (
      <Banner
        tone="danger"
        title={loadError.message}
        action={
          <Button size="sm" variant="secondary" onClick={() => setAttempt((n) => n + 1)}>
            Try again
          </Button>
        }
      >
        {loadError.who}
      </Banner>
    );
  }

  if (!data) {
    return (
      <div className="app-proposal" aria-busy="true" aria-label="Loading proposal">
        <Skeleton width="30%" height={22} />
        <Skeleton width="80%" height={38} />
        <Skeleton width="45%" height={16} />
        <Skeleton height={96} />
        <Skeleton height={160} />
      </div>
    );
  }

  const resolution: Outcome | null = outcome ?? (data.resolved ? { approved: data.resolved.approved, result: data.resolved.result, ...(data.resolved.link ? { link: data.resolved.link } : {}) } : null);
  const kindLabel = KIND_LABEL[data.kind] ?? data.kind;
  const danger = data.kind === 'offboard';
  const when = relativeTime(data.proposed_at, now ?? clockNow);

  return (
    <div className="app-proposal">
      {resolution && (
        <Banner
          tone={resolution.approved ? 'success' : 'info'}
          title={resolution.result}
          action={
            resolution.link ? (
              <Button size="sm" variant="secondary" onClick={() => void navigate(routerPath(resolution.link ?? '/', base))}>
                {linkLabel(data.kind, data.summary)}
              </Button>
            ) : undefined
          }
        >
          {resolution.approved ? 'You approved this proposal.' : 'You denied this proposal.'}
        </Banner>
      )}

      <header className="app-proposal-head">
        <div className="app-proposal-badges">
          <Badge tone="neutral">{kindLabel}</Badge>
          {resolution ? (
            <Badge tone={resolution.approved ? 'success' : 'neutral'}>{resolution.approved ? 'Approved' : 'Denied'}</Badge>
          ) : (
            <Badge tone="signal">Needs your approval</Badge>
          )}
        </div>
        <Text as={desktop ? 'h1' : 'p'} variant="title" className="app-proposal-title">
          {data.title}
        </Text>
        <div className="app-proposal-by">
          <AgentAvatar name={data.proposer.name} size={24} color={identityColorFor(data.proposer.slug)} />
          <Text variant="caption" tone="muted">
            {'Proposed by ' + data.proposer.name + (when ? ' · ' + agoLine(when) : '')}
          </Text>
        </div>
      </header>

      {reasonHtml && <Prose html={reasonHtml} className="app-proposal-reason" />}
      <Attached items={data.attachments} />

      <Summary p={data} />

      {danger && data.offboard_sentence && !resolution && (
        <Banner tone="info" title="Nothing is deleted">
          {data.offboard_sentence}
        </Banner>
      )}

      {data.docs.map((d) => (
        <DocBlock key={d.key} doc={d} />
      ))}

      {!resolution && (
        <section className="app-proposal-decision" aria-label="Your decision">
          {actionError && (
            <Banner tone="danger" title={actionError.message}>
              {actionError.who}
            </Banner>
          )}
          <TextField
            label={'Note to ' + data.proposer.name + ' (optional)'}
            multiline
            rows={3}
            placeholder={NOTE_PLACEHOLDER[data.kind] ?? 'Go ahead, and keep me posted.'}
            value={message}
            onChange={(e) => setMessage(e.currentTarget.value)}
          />
          {files.length > 0 && (
            <ul className="app-proposal-picked" aria-label="Files to send">
              {files.map((f, i) => (
                <li key={f.name + '#' + i}>
                  <Button
                    size="sm"
                    variant="secondary"
                    icon={<Icon name="x" />}
                    aria-label={'Remove ' + f.name}
                    title={'Remove ' + f.name}
                    disabled={busy !== null}
                    onClick={() => setFiles((prev) => prev.filter((_, j) => j !== i))}
                  >
                    {f.name}
                  </Button>
                </li>
              ))}
            </ul>
          )}
          <div className="app-proposal-actions">
            <Button variant="ghost" icon={<Icon name="paperclip" />} disabled={busy !== null} onClick={() => void attach()}>
              Attach
            </Button>
            <span className="app-proposal-spacer" />
            <Button variant="secondary" loading={busy === 'deny'} disabled={busy !== null} onClick={() => void answer('deny')}>
              Deny
            </Button>
            <Button variant={danger ? 'danger' : 'primary'} loading={busy === 'approve'} disabled={busy !== null} onClick={() => void answer('approve')}>
              {approveLabel(data)}
            </Button>
          </div>
        </section>
      )}

      {resolution && (
        <div className="app-proposal-after">
          <Link to="/" className="app-proposal-home">
            Back to Home
            <Icon name="arrow-right" />
          </Link>
        </div>
      )}
    </div>
  );
}
