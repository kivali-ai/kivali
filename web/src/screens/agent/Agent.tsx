import type { ReactNode } from 'react';
import { useParams } from 'react-router';
import { AgentPage } from './AgentPage';
import type { AgentTab } from './AgentPage';
import { About } from './About';
import { Background } from './Background';
import { Chat } from './Chat';
import { Chats } from './Chats';
import { PastChat } from './PastChat';
import { Subagent } from './Subagent';

export type AgentSection = 'chat' | 'background' | 'about' | 'chats' | 'past-chat' | 'subagent';

const TAB_OF: Record<Exclude<AgentSection, 'subagent'>, AgentTab> = {
  chat: 'chat',
  background: 'background',
  about: 'about',
  chats: 'chats',
  'past-chat': 'chats',
};

function body(section: Exclude<AgentSection, 'subagent'>, slug: string): ReactNode {
  if (section === 'chat') return <Chat key={slug} />;
  let tab: ReactNode;
  switch (section) {
    case 'background':
      tab = <Background />;
      break;
    case 'about':
      tab = <About />;
      break;
    case 'chats':
      tab = <Chats />;
      break;
    case 'past-chat':
      tab = <PastChat />;
      break;
  }
  // AgentPage hands the body to the Tabs port, which renders it as the current tab's labelled panel.
  return <div className="app-tab-panel">{tab}</div>;
}

/**
 * Every page under /agents/:slug. The four tabs share AgentPage (header and tabs); a background task's
 * session is its own page with no agent header.
 */
export function Agent({ section }: { section: AgentSection }) {
  const { slug = '', id = '' } = useParams();
  if (section === 'subagent') return <Subagent key={slug + '/' + id} />;
  return (
    <AgentPage key={slug} slug={slug} tab={TAB_OF[section]}>
      {body(section, slug)}
    </AgentPage>
  );
}
