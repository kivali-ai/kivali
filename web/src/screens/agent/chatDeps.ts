import { createContext, useContext } from 'react';
import { createEventStream } from '../../api/sse';
import type { EventStream } from '../../api/sse';

/**
 * The chat's seams: how it opens an agent stream and what time it is. Production uses a real EventSource
 * and the system clock; tests provide a fake stream they drive by hand and a fixed clock.
 */
export interface ChatDeps {
  openStream(url: string): EventStream;
  now(): number;
}

// The agent stream is opened and closed by the chat itself (it follows the turn and the org snapshot), so the wrapper's own visibility handling is switched off with an always-visible document.
const alwaysVisible = {
  visibilityState: 'visible' as DocumentVisibilityState,
  addEventListener: () => {},
  removeEventListener: () => {},
};

export const defaultChatDeps: ChatDeps = {
  openStream: (url) => createEventStream(url, { doc: alwaysVisible }),
  now: () => Date.now(),
};

export const ChatDepsContext = createContext<ChatDeps>(defaultChatDeps);

export function useChatDeps(): ChatDeps {
  return useContext(ChatDepsContext);
}
