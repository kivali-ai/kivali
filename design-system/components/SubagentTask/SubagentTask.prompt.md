Work an agent handed to a short-lived subagent: one block per task, inside the agent's `Message`.

```jsx
<SubagentTask title="Compare three seed suppliers" state="running" model="sonnet" effort="medium" elapsed="1m 12s" />
```

- Header, always visible: what was delegated (`title`), the `model` and `effort` it runs with as a quiet mono pill ("sonnet · medium"), the `elapsed` time, and its state with the `AgentState` vocabulary (`running`, `done`, `errored`).
- It starts collapsed in every state; the header alone says what it is, what it runs on and where it stands. Opened while running, it shows `latest`: one line naming the subagent's most recent step ("Reading the datasheet, page 4 of 9"), followed by the steps so far as children (`ToolCall`, `Thinking`). The full step-by-step lives in the session, not the chat.
- Opened when done, it shows the `result`: the subagent's final answer, clamped to four lines with Show all. The result is what the parent agent received, so it's the thing worth reading.
- When it fails, the header says Failed in `danger`; opened, it shows the `error`.
- `sessionHref` adds "Open full session", the complete transcript. Several tasks started together sit in a `SubagentGroup`.
