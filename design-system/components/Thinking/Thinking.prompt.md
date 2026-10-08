An agent's reasoning between turns, collapsed by default, like every block in chat.

```jsx
<Thinking active={false} seconds={4}>Checked forecast before advising.</Thinking>
```

- `active` shows the running dots with "Thinking"; once done, it becomes "Thought for 12s" (`seconds`) with a quiet brain icon. The person can open it to read the reasoning (children), set in italic behind a thin rule.
- Never show reasoning open by default in chat; it's there for people who want to check.
