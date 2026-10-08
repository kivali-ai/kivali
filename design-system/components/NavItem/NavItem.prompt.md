One entry in the sidebar: a place (Home, Team, Work, Graph, Org) or an agent in the org tree.

```jsx
<NavItem icon="inbox" label="Inbox" count={3} attention active />
```

- `icon` for places, or `lead` for anything else (an `AgentAvatar` at 20px for agents). `label` is required.
- `count` shows a quiet number; with `attention` it becomes a `signal` pill, reserved for things waiting on the person (the inbox). `count` can also hold a compact `AgentState`.
- `active` marks the current page with a raised `paper-raised` tile; `depth` indents tree levels by 16px.
- `href` renders a link, otherwise a button with `onClick`.
