import { useMemo, useState } from 'react';
import { useNavigate, useParams } from 'react-router';
import type { PastChatDetail, PastChats } from '../../api/types.gen';
import { BREAKPOINT_ONE_COLUMN, Badge, Banner, Button, ChatPager, DocDiff, Icon, ListRow, Prose, Skeleton, Text, mediaQuery } from '../../ds';
import { useMediaQuery } from '../../lib/useMediaQuery';
import { renderMarkdown } from '../../lib/markdown';
import { useAgentPage } from './AgentPage';
import { useChatDeps } from './chatDeps';
import { chatMeta } from './Chats';
import { Transcript, keyRows } from './Transcript';
import { rangeLabel, stripProvenance, useFetched } from './tabsShared';
import '../../styles/agent-tabs.css';

/** A note as the reader sees it: the memory's provenance marker ("[[ep:…]]") is bookkeeping, not text. */
export function noteText(note: string): string {
  return stripProvenance(note).trim();
}

/** One past chat: its summary first, the transcript one button away. */
export function PastChat() {
  const { ts = '' } = useParams();
  return <PastChatBody key={ts} ts={ts} />;
}

function PastChatBody({ ts }: { ts: string }) {
  const { slug, name, archived } = useAgentPage();
  const navigate = useNavigate();
  const deps = useChatDeps();
  const compact = !useMediaQuery(mediaQuery(BREAKPOINT_ONE_COLUMN));
  const base = '/api/v1/agents/' + encodeURIComponent(slug) + '/chats';
  const { data, error, loading } = useFetched<PastChatDetail>(base + '/' + encodeURIComponent(ts));
  const list = useFetched<PastChats>(base);
  const [open, setOpen] = useState(false);
  const rows = useMemo(() => (open ? keyRows(data?.rows ?? []) : []), [open, data]);
  const digest = useMemo(() => renderMarkdown(data?.summary.digest_md ?? ''), [data]);

  if (loading) {
    return (
      <div className="app-past" aria-busy="true">
        <Skeleton height={28} width="50%" />
        <Skeleton height={96} />
      </div>
    );
  }
  if (!data) {
    return (
      <Banner tone="danger" title={error?.message ?? 'This past chat could not be read.'}>
        {error?.who}
      </Banner>
    );
  }

  const { summary } = data;
  const now = deps.now();
  const range = rangeLabel(summary.range.from, summary.range.to, now);
  const chats = list.data?.chats ?? [];
  const position = chats.findIndex((c) => c.ts === summary.ts);
  const total = Math.max(chats.length, 1);
  // Older is a larger position in the newest-first list; the pager counts from the oldest chat.
  const index = position >= 0 ? chats.length - position : total;
  const go = (to: string | undefined) => {
    if (to) void navigate('/agents/' + encodeURIComponent(slug) + '/chats/' + encodeURIComponent(to));
  };
  const notes = summary.memory_added.map(noteText).filter(Boolean);
  const hasHabits = summary.habits_diff.before.trim() !== '' || summary.habits_diff.after.trim() !== '';

  return (
    <div className="app-past">
      <ChatPager
        title={'Past chat' + (range ? ' · ' + range : '')}
        index={index}
        total={total}
        compact={compact}
        onOlder={() => go(data.prev_ts)}
        onNewer={() => go(data.next_ts)}
        {...(archived ? {} : { onCurrent: () => void navigate('/agents/' + encodeURIComponent(slug)) })}
      />
      <div className="app-past-head">
        <Text as="h2" variant="heading">
          {summary.title}
        </Text>
        <Text as="p" variant="label" tone="muted">
          {chatMeta(summary, now)}
        </Text>
      </div>

      <section className="app-past-section" aria-labelledby="past-happened">
        <Text as="h3" variant="label" tone="muted" id="past-happened">
          What happened
        </Text>
        {digest ? (
          <Prose html={digest} />
        ) : (
          <Text as="p" variant="body" tone="muted">
            The summary is still being written.
          </Text>
        )}
      </section>

      <section className="app-past-section" aria-labelledby="past-notes">
        <Text as="h3" variant="label" tone="muted" id="past-notes">
          Notes it learned
        </Text>
        {notes.length === 0 ? (
          <Text as="p" variant="body" tone="muted">
            {name} added nothing to its memory from this chat.
          </Text>
        ) : (
          <div className="app-tabs-list">
            <div className="app-tabs-rows">
              {notes.map((n, i) => (
                <ListRow
                  key={i + ':' + n}
                  title={<span className="app-past-note">{n}</span>}
                  trail={<Badge tone="cobalt">Learned</Badge>}
                />
              ))}
            </div>
          </div>
        )}
      </section>

      <section className="app-past-section" aria-labelledby="past-habits">
        <Text as="h3" variant="label" tone="muted" id="past-habits">
          Habits before and after
        </Text>
        {hasHabits ? (
          <DocDiff before={summary.habits_diff.before} after={summary.habits_diff.after} />
        ) : (
          <Text as="p" variant="body" tone="muted">
            It has no habits written down.
          </Text>
        )}
      </section>

      <div className="app-past-transcript">
        <Button variant="secondary" icon={<Icon name={open ? 'chevron-down' : 'arrow-right'} />} aria-expanded={open} onClick={() => setOpen((o) => !o)}>
          Read the transcript
        </Button>
        {open &&
          (rows.length === 0 ? (
            <Text as="p" variant="body" tone="muted">
              This chat has no messages.
            </Text>
          ) : (
            <Transcript rows={rows} agent={{ slug, name }} now={summary.range.to || now} readOnly />
          ))}
      </div>
      {list.error && !list.data && (
        <Banner tone="warning" title={list.error.message}>
          {list.error.who}
        </Banner>
      )}
    </div>
  );
}
