One row in a list: agents, files, chats, skills. The default way to show many things; cards are for the few that need a decision.

```jsx
<ListRow lead={<AgentAvatar name="Bookkeeper" role="calculator" size={32} />} title="Bookkeeper" meta={<AgentState state="idle" />} trail="2m" />
```

- `lead` (avatar or icon), `title`, `meta` (one muted line that can hold `AgentState` or `Badge`), `trail` (time, a count, a small action).
- With `href` or `onClick` the whole row is the target; `selected` tints it `cobalt-soft`.
- Rows are separated by `line` hairlines. Put a list of rows inside one raised container rather than giving each row its own card.
- `expanded`: for a row that opens content below it; sets `aria-expanded` on the row button.
