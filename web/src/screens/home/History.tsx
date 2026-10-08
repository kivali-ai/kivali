import { useCallback, useEffect, useRef, useState } from 'react';
import { ApiError, apiGet } from '../../api/client';
import type { HistoryMessage, HistoryResponse, HistoryThread } from '../../api/types.gen';
import { Banner, Button, EmptyState, Icon, ListRow, Text } from '../../ds';
import { relativeTime } from '../../lib/time';
import { asSentence } from '../../state/home';
import { Attachments, Body, Face, RowSkeletons } from './parts';

export const HISTORY_PAGE = 20;

function historyUrl(before?: string): string {
  const q = new URLSearchParams({ limit: String(HISTORY_PAGE) });
  if (before) q.set('before', before);
  return '/api/v1/home/history?' + q.toString();
}

function decisionWord(d: string | undefined): string {
  if (!d) return '';
  return d.charAt(0).toUpperCase() + d.slice(1);
}

function Reply({ m, now }: { m: HistoryMessage; now: number }) {
  return (
    <div className="app-home-reply">
      <div className="app-home-reply-head">
        <Face who={m.from} size={24} />
        <Text variant="caption">{m.from.name}</Text>
        <Text variant="caption" tone="muted">
          {m.label} · {relativeTime(m.at, now)}
        </Text>
      </div>
      <Body markdown={m.body_md} />
      <Attachments items={m.attachments} />
    </div>
  );
}

function ThreadRow({ t, now, expanded, onToggle }: { t: HistoryThread; now: number; expanded: boolean; onToggle(): void }) {
  const r = t.request;
  return (
    <div className="app-home-item" data-path={t.path}>
      <ListRow
        lead={<Face who={r.from} size={32} />}
        title={r.title}
        meta={
          <>
            <span>{r.from.name}</span>
            <span>{r.label}</span>
            <span>{relativeTime(t.last_activity, now)}</span>
          </>
        }
        trail={
          <>
            {decisionWord(t.decision)}
            <Icon name={expanded ? 'chevron-up' : 'chevron-down'} />
          </>
        }
        onClick={onToggle}
        expanded={expanded}
      />
      {expanded && (
        <div className="app-home-expand">
          <Body markdown={r.body_md} />
          <Attachments items={r.attachments} />
          {t.replies.map((m) => (
            <Reply key={m.path} m={m} now={now} />
          ))}
        </div>
      )}
    </div>
  );
}

export interface HistoryProps {
  now: number;
  /** Changes when messages land (the snapshot's messages_total); the first page reloads. */
  version: number;
}

/** Threads you have dealt with, newest activity first, paged with Show more. */
export function History({ now, version }: HistoryProps) {
  const [threads, setThreads] = useState<HistoryThread[] | null>(null);
  const [total, setTotal] = useState(0);
  const [next, setNext] = useState<string | undefined>(undefined);
  const [loadingMore, setLoadingMore] = useState(false);
  const [error, setError] = useState<ApiError | null>(null);
  const [expanded, setExpanded] = useState<ReadonlySet<string>>(new Set());
  const seq = useRef(0);

  const loadFirst = useCallback(async () => {
    const mine = ++seq.current;
    try {
      const page = await apiGet<HistoryResponse>(historyUrl());
      if (mine !== seq.current) return;
      setThreads(page.threads);
      setTotal(page.total);
      setNext(page.next_before);
      setLoadingMore(false);
      setError(null);
    } catch (err) {
      if (mine !== seq.current) return;
      setError(err instanceof ApiError ? err : new ApiError(0, 'History could not load.', 'Reload the page.'));
    }
  }, []);

  useEffect(() => {
    void loadFirst();
    return () => {
      seq.current++;
    };
  }, [loadFirst, version]);

  const more = async () => {
    if (!next) return;
    const mine = ++seq.current;
    setLoadingMore(true);
    try {
      const page = await apiGet<HistoryResponse>(historyUrl(next));
      if (mine !== seq.current) return;
      setThreads((prev) => {
        const seen = new Set((prev ?? []).map((t) => t.path));
        return [...(prev ?? []), ...page.threads.filter((t) => !seen.has(t.path))];
      });
      setTotal(page.total);
      setNext(page.next_before);
      setError(null);
    } catch (err) {
      if (mine !== seq.current) return;
      setError(err instanceof ApiError ? err : new ApiError(0, 'History could not load.', 'Reload the page.'));
    } finally {
      if (mine === seq.current) setLoadingMore(false);
    }
  };

  const toggle = (path: string) =>
    setExpanded((prev) => {
      const n = new Set(prev);
      if (n.has(path)) n.delete(path);
      else n.add(path);
      return n;
    });

  return (
    <section className="app-home-section" aria-labelledby="home-history">
      <div className="app-home-head">
        <Text as="h2" variant="heading" id="home-history">
          History
        </Text>
        {total > 0 && (
          <Text variant="label" tone="muted">
            {total === 1 ? '1 thread' : total + ' threads'}
          </Text>
        )}
      </div>
      {error && (
        <Banner tone="danger" title={asSentence(error.message)}>
          {error.who}
        </Banner>
      )}
      <div className="app-home-list">
        <div className="app-home-rows">
          {threads === null ? (
            !error && <RowSkeletons />
          ) : threads.length === 0 ? (
            <EmptyState title="No history yet">What you approve, answer and release shows up here.</EmptyState>
          ) : (
            threads.map((t) => <ThreadRow key={t.path} t={t} now={now} expanded={expanded.has(t.path)} onToggle={() => toggle(t.path)} />)
          )}
        </div>
      </div>
      {next && threads && (
        <div className="app-home-more">
          <Button variant="secondary" loading={loadingMore} onClick={() => void more()}>
            Show more
          </Button>
        </div>
      )}
    </section>
  );
}

