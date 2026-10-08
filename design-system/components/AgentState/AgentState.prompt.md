An agent's run state as three small dots plus a word: the logo's honey dots at work.

```jsx
<AgentState state="running" />
```

- `state`: `running` (dots bounce in `signal`, "Working"), `idle` (a single resting dot), `queued` (three hollow `cobalt` rings waiting in line), `blocked` (two dots stopped at a wall, in `signal-ink`), `held` (a pause mark), `errored` ("Needs help" in `danger` with an icon), `done` (`success` check), `cancelled`.
- `label` overrides the word ("Writing report"); keep it short.
- `compact` shows the dots only, with the word as the accessible name and tooltip. Use it in dense lists and the org tree; use the full form everywhere else.
- When the person prefers reduced motion the running dots stop bouncing and fade slowly in place, staggered, so a running agent still reads as running. Remote Desktop on Windows reports reduced motion.
- Only `running` moves. Every state that is not normal work has a shape of its own (a wall, a pause mark, an icon), so no still state can be mistaken for a frame of the running animation.
- The glyphs are a small vocabulary, not a fixed list: moving (running), resting (idle), waiting in line (queued), stopped by something (blocked), paused by a person (held), a problem (errored), finished (done), withdrawn (cancelled). If the product's state list changes, map each new state to the closest meaning and add a word; add a new glyph only for a genuinely new meaning.
