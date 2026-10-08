import { useMemo } from 'react';
import type { PastChat, PastChats } from '../../api/types.gen';
import { Banner, EmptyState, Icon, ListRow, Skeleton, Text } from '../../ds';
import { useAgentPage } from './AgentPage';
import { useChatDeps } from './chatDeps';
import { messagesLabel, monthLabel, rangeLabel, tokensLabel, useAppHref, useFetched } from './tabsShared';
import '../../styles/agent-tabs.css';

/** "12 to 26 Sept · 212 messages · ~188k tokens": what a past chat row says under its title. */
export function chatMeta(chat: Pick<PastChat, 'range' | 'messages' | 'tokens'>, now: number): string {
  return [rangeLabel(chat.range.from, chat.range.to, now), messagesLabel(chat.messages), tokensLabel(chat.tokens)].filter(Boolean).join(' · ');
}

/** Past chats, newest first, under a month heading as the canvas draws them; the current chat leads. */
export function Chats() {
  const { slug, name, detail, archived } = useAgentPage();
  const deps = useChatDeps();
  const href = useAppHref();
  const { data, error, loading } = useFetched<PastChats>('/api/v1/agents/' + encodeURIComponent(slug) + '/chats');
  const now = deps.now();

  const groups = useMemo(() => {
    const out: { month: string; chats: PastChat[] }[] = [];
    for (const chat of data?.chats ?? []) {
      const month = monthLabel(chat.range.from || chat.range.to, now);
      const last = out[out.length - 1];
      if (last && last.month === month) last.chats.push(chat);
      else out.push({ month, chats: [chat] });
    }
    return out;
  }, [data, now]);

  if (loading) {
    return (
      <div className="app-chats" aria-busy="true">
        <Skeleton height={56} />
        <Skeleton height={56} />
      </div>
    );
  }
  if (!data) {
    return (
      <Banner tone="danger" title={error?.message ?? 'The past chats could not be read.'}>
        {error?.who}
      </Banner>
    );
  }

  const tokens = detail?.context.tokens ?? 0;
  return (
    <div className="app-chats">
      {!archived && (
        <div className="app-tabs-list">
          <div className="app-tabs-rows">
            <ListRow
              lead={<Icon name="message-square" />}
              title="Current chat"
              meta={tokens > 0 ? tokensLabel(tokens) + ' so far' : 'Open the chat with ' + name}
              trail={<Icon name="chevron-right" />}
              href={href('/agents/' + encodeURIComponent(slug))}
            />
          </div>
        </div>
      )}
      {groups.length === 0 ? (
        <div className="app-tabs-list">
          <EmptyState title="No past chats yet">{'A chat moves here when ' + name + ' folds it into what it remembers.'}</EmptyState>
        </div>
      ) : (
        groups.map((g) => (
          <section key={g.month} className="app-chats-group" aria-label={g.month}>
            <Text as="h2" variant="label" tone="muted">
              {g.month}
            </Text>
            <div className="app-tabs-list">
              <div className="app-tabs-rows">
                {g.chats.map((chat) => (
                  <ListRow
                    key={chat.ts}
                    lead={<Icon name="history" />}
                    title={chat.title}
                    meta={chatMeta(chat, now)}
                    trail={<Icon name="chevron-right" />}
                    href={href('/agents/' + encodeURIComponent(slug) + '/chats/' + encodeURIComponent(chat.ts))}
                  />
                ))}
              </div>
            </div>
          </section>
        ))
      )}
    </div>
  );
}
