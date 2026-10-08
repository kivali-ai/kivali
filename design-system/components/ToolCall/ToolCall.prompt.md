One tool an agent used: collapsed to a single line, openable to see what went in and what came out.

```jsx
<ToolCall name="garden_notes" summary="bed 3, last watered" status="done" duration="0.4s" input={{ bed: 3 }} output="Watered Friday" />
```

- `name` (mono), a short `summary` of what it acted on, `status` (`running`, `done`, `error`) shown with the `AgentState` dots, and `duration` once finished.
- `input` and `output` accept text or objects (shown as formatted JSON); `error` replaces the output and turns the block's edge and payload `danger`. Failed calls stay collapsed too; the header's Failed state and red edge flag them.
- Several consecutive calls stack with an 8px gap. Keep them inside the agent's `Message`, below its text.
- Opened while running, it shows the input it was called with and "Waiting for output" with running dots; a call with no input or output says so rather than showing an empty box.
