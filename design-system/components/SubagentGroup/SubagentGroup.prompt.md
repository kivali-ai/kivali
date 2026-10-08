Several subagents an agent started at once, gathered under one line.

```jsx
<SubagentGroup tasks={[{ title: 'Summarize rainfall', state: 'done' }]} />
```

- The header counts them and sums up where they stand ("1 running · 2 done", with "failed" when any failed); its state glyph is the worst of the group.
- It starts collapsed; the header's counts and state say enough. Inside, each task is a `SubagentTask`, also collapsed.
- Pass `tasks` (each the props of a `SubagentTask`), or `SubagentTask` children for full control.
