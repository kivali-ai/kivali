import { useEffect, useMemo, useRef, useState } from 'react';
import { useParams } from 'react-router';
import type { SubagentTranscript } from '../../api/types.gen';
import { useFrameChrome } from '../../app/chrome';
import { AgentState, Badge, Banner, Skeleton, Text } from '../../ds';
import { flattenTree } from '../../state/org';
import { useOrg } from '../../state/OrgProvider';
import { useNow } from '../home/useNow';
import { useChatDeps } from './chatDeps';
import { Transcript, keyRows } from './Transcript';
import { SESSION_STATE, elapsedLabel, modelLabel, useFetched } from './tabsShared';
import '../../styles/agent-tabs.css';

/** "step 3" for a number, the step's own words otherwise ("Read the logs"). */
function stepPhrase(step: string): string {
  return /^\d+$/.test(step.trim()) ? 'step ' + step.trim() : step;
}

/**
 * One background task, read only: who sent it and for which step, what state it is in, then its transcript.
 * While it runs the page follows its stream. The stream announces every row of the task's file with an id that
 * is the count of rows sent so far; each one, and each meta change, means the transcript has moved, so the
 * page reads it again (one read at a time) rather than rebuilding rows from the raw file lines. The count of
 * rows received from the file is what a reopened stream resumes from, never anything counted in the page.
 */
export function Subagent() {
  const { slug = '', id = '' } = useParams();
  const { org } = useOrg();
  const deps = useChatDeps();
  const path = '/api/v1/agents/' + encodeURIComponent(slug) + '/subagents/' + encodeURIComponent(id);
  const { data, error, loading, refresh } = useFetched<SubagentTranscript>(path);
  const received = useRef(0);

  const meta = data?.meta;
  const parentSlug = meta?.parent || slug;
  const parentName = useMemo(() => flattenTree(org.tree).find((n) => n.slug === parentSlug)?.name ?? parentSlug, [org.tree, parentSlug]);
  const running = meta?.state === 'running';

  useFrameChrome({
    title: meta?.description || 'Background task',
    tabBar: false,
    back: { to: '/agents/' + slug + '/background', label: parentName + ' · Background' },
    width: 'transcript',
  });

  // A hidden tab lets the stream go (the chat deps' streams do not follow visibility themselves); coming back
  // opens a new one from the rows already received and reads the transcript at once.
  const [visible, setVisible] = useState(() => document.visibilityState !== 'hidden');
  useEffect(() => {
    const onVisibility = () => setVisible(document.visibilityState !== 'hidden');
    document.addEventListener('visibilitychange', onVisibility);
    return () => document.removeEventListener('visibilitychange', onVisibility);
  }, []);
  const opened = useRef(0);

  useEffect(() => {
    if (!running || !visible) return;
    // A reopen (after the tab was hidden) catches up on what happened meanwhile; the first open has the mount's read.
    if (opened.current++ > 0) void refresh();
    const from = received.current;
    const stream = deps.openStream('/agents/' + encodeURIComponent(slug) + '/subagents/' + encodeURIComponent(id) + '/stream' + (from > 0 ? '?from=' + from : ''));
    stream.on('chat_message', (_data, ev) => {
      // The server ids each row with the count of file rows sent so far; that, not anything the page drew, is
      // where a reopened stream resumes. A row without an id counts one.
      const n = Number(ev.lastEventId);
      received.current = Number.isInteger(n) && n > 0 ? Math.max(received.current, n) : received.current + 1;
      void refresh();
    });
    stream.on('meta', () => void refresh());
    stream.on('done', () => {
      stream.stop();
      void refresh();
    });
    stream.start();
    return () => stream.stop();
  }, [running, visible, deps, slug, id, refresh]);

  const now = useNow(running ? 1000 : 60_000, deps.now);
  const rows = useMemo(() => keyRows(data?.rows ?? []), [data]);

  if (loading) {
    return (
      <div className="app-sub" aria-busy="true">
        <Skeleton height={28} width="60%" />
        <Skeleton height={18} width="40%" />
        <Skeleton height={96} />
      </div>
    );
  }
  if (!data || !meta) {
    return (
      <Banner tone="danger" title={error?.message ?? 'This background task could not be read.'}>
        {error?.who}
      </Banner>
    );
  }

  const seconds = meta.started > 0 ? Math.max(0, ((meta.ended ?? now) - meta.started) / 1000) : 0;
  const stateLine = 'Background task for ' + parentName + (meta.step ? ' · ' + stepPhrase(meta.step) : '');

  return (
    <div className="app-sub">
      <div className="app-sub-head">
        <Text as="h1" variant="heading" className="app-page-title">
          {meta.description}
        </Text>
        <div className="app-sub-line">
          <AgentState state={SESSION_STATE[meta.state]} />
          <Text variant="caption" tone="muted">
            {stateLine}
          </Text>
          <Badge variant="outline" mono>
            {modelLabel(meta.model, meta.effort)}
          </Badge>
          {meta.started > 0 && (
            <Text variant="label" tone="muted">
              {elapsedLabel(seconds, running)}
            </Text>
          )}
        </div>
        {meta.state === 'errored' && meta.error && (
          <Text as="p" variant="caption" tone="danger">
            {meta.error}
          </Text>
        )}
        {running && meta.activity && (
          <Text as="p" variant="caption" tone="muted">
            {meta.activity}
          </Text>
        )}
      </div>
      {error && (
        <Banner tone="warning" title={error.message}>
          {error.who}
        </Banner>
      )}
      {rows.length === 0 ? (
        <Text as="p" variant="body" tone="muted">
          {running ? 'Waiting for its first message.' : 'This task recorded no messages.'}
        </Text>
      ) : (
        <Transcript rows={rows} agent={{ slug: meta.id, name: meta.description }} senderSlug={meta.parent} now={now} readOnly />
      )}
    </div>
  );
}
