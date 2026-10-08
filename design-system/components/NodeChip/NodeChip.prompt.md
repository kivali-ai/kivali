One node of the knowledge graph: a decision, a requirement, or a plain artifact.

```jsx
<NodeChip id="decision/seed-supplier" kind="decision" summary="Buy from the local seed co-op" owner="iris" />
```

- `kind` sets the glyph: `decision` is a filled diamond, `requirement` an outlined square, `artifact` a faint circle. `id` (mono) and a one-line `summary`.
- `owner` is the owning agent's identity color, shown as a small square so clusters read by color; pass `ownerName` for its tooltip.
- `status`: `active`; `superseded` (struck through and faded: still findable, no longer binding); `flagged` (a `signal-soft` tint with a warning icon: something needs attention); `unresolved` (a dashed edge with a `danger` link icon: it rests on something that doesn't exist).
- `selected` outlines it in `cobalt`. Links between nodes follow the rules on the Graph links card.
