One turn in a conversation: from an agent, from a person, or from the system.

```jsx
<Message from={{ kind: 'agent', name: 'Garden advisor', role: 'sprout', color: 'olive' }} time="09:14">Bed 3 is dry.</Message>
```

- `from` is `{kind, name, ...}`. Agents (`kind: "agent"`) pass `role` and `color` for their `AgentAvatar` and write straight onto the page; people (`kind: "person"`) get a `PersonAvatar` and their words sit in a raised bubble, so it's always clear who's speaking. `kind: "system"` is a centered mono line between rules for events like rotations and handoffs.
- `time` is short ("6:02"); the full timestamp belongs in a tooltip. Children are the content, usually `Prose` for markdown, followed by any `Thinking`, `ToolCall` or `SubagentTask` blocks in the order they happened.
- `streaming` adds a blinking honey caret at the end while an agent is still writing.
- `model` shows the model and effort that wrote an agent reply as an outline mono badge at the foot ("opus · high"). Pass it on every model-written message, so a chat that switches models stays readable.
- `pending` marks a person's message sent mid-turn and not yet delivered. The bubble turns to paper with a dashed ink-faint edge, the head gets a cobalt "Pending" badge, and a line reads "Waiting for a moment to deliver" with Delete (secondary) and Send now (primary).
- After Send now, render a system message "Paused to deliver your message", then the message as normal.
- Running dots (Thinking, "Waiting on 2 tasks") and the streaming caret never appear on the same turn. Dots sit once at the bottom of the turn; the caret means text is arriving.
- `sendNowLoading`: Send now was clicked and delivery has not landed; the button shows its loading dots and Delete is disabled. Delete renders only when `onDelete` is given.
