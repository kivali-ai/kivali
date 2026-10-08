A raised container for one thing: an inbox item, a review, an agent, an assignment.

```jsx
<Card title="Garden advisor" meta="Gardening · 214 notes" actions={<Button size="sm" variant="primary">Open</Button>}>Checks the forecast each morning.</Card>
```

- `title` (Space Grotesk), `meta` (a line of muted details), `actions` (usually `sm` buttons, the primary last), and the body as children.
- `collapsible` turns it into a disclosure: the header toggles the body; `defaultOpen` sets the first state.
- `tone="attention"` gives it a solid `ink` edge for the one card that needs the person now.
- Cards sit on `paper` with a `line` border and `shadow-sm`. Don't nest cards; use rows inside a card instead.
- Keep the two kinds of status apart. The card's own status (needs your approval, approved, failed) is a `Badge`. An agent's run state (`AgentState`) appears only beside that agent's name, and only when it matters to the decision.
- A card whose status needs the person shows it as a `Badge` in the meta line; a settled card says it in plain words with the time ("Done yesterday"). See When to use a badge in the Badge guidelines.
