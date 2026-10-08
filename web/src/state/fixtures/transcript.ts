// Chat fixtures. The Go golden files are what the server really emits; the transcript-*.json files are
// recorded event sequences, each naming what it was written from.
import agentDetailJson from '../../../../internal/web/apitypes/testdata/agent_detail.json';
import chatJson from '../../../../internal/web/apitypes/testdata/chat.json';
import chatMarkerJson from '../../../../internal/web/apitypes/testdata/chat_marker.json';
import chatMessageJson from '../../../../internal/web/apitypes/testdata/chat_message.json';
import chatMessageTaskResultJson from '../../../../internal/web/apitypes/testdata/chat_message_task_result.json';
import messagePostJson from '../../../../internal/web/apitypes/testdata/message_post_response.json';
import newChatJson from '../../../../internal/web/apitypes/testdata/new_chat_response.json';
import pendingDeliveredJson from '../../../../internal/web/apitypes/testdata/pending_delivered.json';
import pendingMessageJson from '../../../../internal/web/apitypes/testdata/pending_message.json';
import pendingRefJson from '../../../../internal/web/apitypes/testdata/pending_ref.json';
import pendingRestoreJson from '../../../../internal/web/apitypes/testdata/pending_restore_response.json';
import pastChatDetailJson from '../../../../internal/web/apitypes/testdata/past_chat_detail.json';
import pastChatsJson from '../../../../internal/web/apitypes/testdata/past_chats.json';
import subagentTranscriptJson from '../../../../internal/web/apitypes/testdata/subagent_transcript.json';
import settingsJson from '../../../../internal/web/apitypes/testdata/chat_settings.json';
import stopJson from '../../../../internal/web/apitypes/testdata/stop_response.json';
import toolCallDetailJson from '../../../../internal/web/apitypes/testdata/tool_call_detail.json';
import type {
  AgentDetail,
  Chat,
  ChatMarker,
  ChatMessageEvent,
  ChatSettings,
  MessagePostResponse,
  NewChatResponse,
  PastChatDetail,
  PastChats,
  PendingDelivered,
  PendingMessage,
  PendingRef,
  PendingRestoreResponse,
  StopResponse,
  SubagentTranscript,
  ToolCallDetail,
} from '../../api/types.gen';
import type { TranscriptAction } from '../transcript';
import deltaReplay from './transcript-delta-replay.json';
import fullTurn from './transcript-full-turn.json';
import modelSwitch from './transcript-model-switch.json';
import pendingSendNow from './transcript-pending-send-now.json';
import rotation from './transcript-rotation.json';
import subagents from './transcript-subagents.json';
import toolLifecycle from './transcript-tool-lifecycle.json';
import turnError from './transcript-turn-error.json';

/**
 * The wire shape of T with its string-literal unions widened to string, which is how a JSON import is
 * typed. `json satisfies Wire<T>` fails the build when a golden file loses a required field, renames one or
 * changes its type; only the enum values themselves go unchecked.
 */
export type Wire<T> = T extends string
  ? string
  : T extends readonly (infer U)[]
    ? Wire<U>[]
    : T extends object
      ? { [K in keyof T]: Wire<T[K]> }
      : T;

const checkedChat = chatJson satisfies Wire<Chat>;
const checkedDetail = agentDetailJson satisfies Wire<AgentDetail>;
export const goSubagentTranscript = subagentTranscriptJson satisfies Wire<SubagentTranscript>;
export const goPastChats = pastChatsJson satisfies Wire<PastChats>;
export const goPastChatDetail = pastChatDetailJson satisfies Wire<PastChatDetail>;
export const goToolCallDetail: ToolCallDetail = toolCallDetailJson satisfies Wire<ToolCallDetail>;

const checkedChatMessage = chatMessageJson satisfies Wire<ChatMessageEvent>;
const checkedChatMessageTaskResult = chatMessageTaskResultJson satisfies Wire<ChatMessageEvent>;

// Checked above; the casts narrow the widened enums back (as index.ts does for the snapshot).
export const goChat = checkedChat as Chat;
export const goAgentDetail = checkedDetail as AgentDetail;
/** The stream's chat_message for a message you sent, and for a background task's report with its row. */
export const goChatMessage = checkedChatMessage as ChatMessageEvent;
export const goChatMessageTaskResult = checkedChatMessageTaskResult as ChatMessageEvent;
// Shapes with no string unions check directly.
export const goChatMarker = chatMarkerJson satisfies ChatMarker;
export const goMessagePost = messagePostJson satisfies MessagePostResponse;
export const goNewChat = newChatJson satisfies NewChatResponse;
export const goPendingDelivered = pendingDeliveredJson satisfies PendingDelivered;
export const goPendingMessage = pendingMessageJson satisfies PendingMessage;
export const goPendingRef = pendingRefJson satisfies PendingRef;
export const goPendingRestore = pendingRestoreJson satisfies PendingRestoreResponse;
export const goChatSettings = settingsJson satisfies ChatSettings;
export const goStop = stopJson satisfies StopResponse;

/** An empty chat for an idle agent, built from the golden file so its shape stays the server's. */
export const emptyChat: Chat = { ...goChat, rows: [], pending: [], running: false, waiting_tasks: 0 };

/** The chat that follows `chat` once a new one has started: empty, and under the next generation. */
export function freshChatAfter(chat: Chat): Chat {
  return { ...emptyChat, rotating: false, generation: chat.generation + '+1', fill: { ...emptyChat.fill, pct: 0, tokens: 0, bucket: 'low' } };
}

/** One recorded step: a stream event, or a local action the screen dispatches (a reopen, the debounce). */
export type Step = { event: string; data: unknown } | { action: 'stream_open' | 'stream_closed' | 'thinking_promote' };

export interface Sequence {
  source: string;
  about: string;
  steps: Step[];
}

export const sequences = {
  deltaReplay: deltaReplay as Sequence,
  fullTurn: fullTurn as Sequence,
  modelSwitch: modelSwitch as Sequence,
  pendingSendNow: pendingSendNow as Sequence,
  rotation: rotation as Sequence,
  subagents: subagents as Sequence,
  toolLifecycle: toolLifecycle as Sequence,
  turnError: turnError as Sequence,
};

/** A step as a reducer action, stamped with `at`. */
export function stepAction(step: Step, at: number): TranscriptAction {
  if ('event' in step) return { type: 'event', name: step.event, data: step.data, at };
  if (step.action === 'stream_closed') return { type: 'stream_closed' };
  if (step.action === 'thinking_promote') return { type: 'thinking_promote' };
  return { type: 'stream_open', at };
}
