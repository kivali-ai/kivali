Where an assignment stands, as a small circle plus a word. Assignments are circles; agents are dots (`AgentState`), so the two never read as each other.

```jsx
<AssignmentState state="blocked" />
```

- `state`: `ready` (a filled center in `cobalt`: can move now), `blocked` (a wall through the circle in `signal-ink`: waiting on children or other assignments), `held` (a pause mark in `signal-ink`), `done` (a filled `success` check), `dropped` (a slash in `ink-muted`).
- These are derived from the tracker, never set by hand: blocked, held and ready follow from the tree. `label` overrides the word; `compact` shows the circle only with the word as its accessible name.
- Like agent states, the list may change; map new states to the nearest meaning before adding a glyph.
