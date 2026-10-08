// Fixtures for the Background, About, Past chats and task pages. The Go golden files are what the server
// really emits; `satisfies Wire<T>` fails the build when one drifts from the generated types.
import agentDocJson from '../../../../internal/web/apitypes/testdata/agent_doc.json';
import agentDocEmptyHabitsJson from '../../../../internal/web/apitypes/testdata/agent_doc_empty_habits.json';
import agentsJson from '../../../../internal/web/apitypes/testdata/agents.json';
import backgroundJson from '../../../../internal/web/apitypes/testdata/background.json';
import backgroundEmptyJson from '../../../../internal/web/apitypes/testdata/background_empty.json';
import type { AgentDoc, AgentsResponse, Background, PastChat, PastChatDetail, PastChats, SubagentTranscript } from '../../api/types.gen';
import type { Wire } from '../../state/fixtures/transcript';
import { goPastChatDetail, goSubagentTranscript } from '../../state/fixtures/transcript';

export const goBackground = backgroundJson satisfies Wire<Background>;
export const goBackgroundEmpty = backgroundEmptyJson satisfies Wire<Background>;
export const goMemoryDoc = agentDocJson satisfies Wire<AgentDoc>;
export const goEmptyHabits = agentDocEmptyHabitsJson satisfies Wire<AgentDoc>;
export const goAgents = agentsJson satisfies Wire<AgentsResponse>;

/** The goldens with their string unions typed. */
export const background = goBackground as Background;
export const backgroundEmpty = goBackgroundEmpty as Background;
export const memoryDoc = goMemoryDoc as AgentDoc;
export const emptyHabits = goEmptyHabits as AgentDoc;
export const agents = goAgents as AgentsResponse;
export const pastChatDetail = goPastChatDetail as PastChatDetail;
export const subagentTranscript = goSubagentTranscript as SubagentTranscript;

/** 22 Sept 2026, 17:00 UTC: the fixed "now" the pages read from the chat deps. */
export const NOW = Date.parse('2026-09-22T17:00:00Z');

const noon = (iso: string) => Date.parse(iso + 'T12:00:00Z');

/** Three past chats across two months, newest first. */
export const pastChats: PastChats = {
  chats: [
    { ts: 'c3', title: 'Hosting quotes and the order', range: { from: noon('2026-09-12'), to: noon('2026-09-20') }, messages: 212, tokens: 188_000 },
    { ts: 'c2', title: 'Login fixes for the web app', range: { from: noon('2026-09-02'), to: noon('2026-09-10') }, messages: 40, tokens: 61_500 },
    { ts: 'c1', title: 'Release 1 post-mortem', range: { from: noon('2026-08-18'), to: noon('2026-09-01') }, messages: 1, tokens: 900 },
  ],
};

export function pastChat(ts: string, over: Partial<PastChatDetail['summary']> = {}, links: { prev_ts?: string; next_ts?: string } = {}): PastChatDetail {
  const base = pastChats.chats.find((c: PastChat) => c.ts === ts) ?? { ts, title: 'A past chat', range: { from: 0, to: 0 }, messages: 0, tokens: 0 };
  return {
    summary: { ...base, digest_md: 'Compared three hosts and chose the cheapest.', memory_added: [], habits_diff: { before: '', after: '' }, ...over },
    rows: [],
    ...links,
  };
}
