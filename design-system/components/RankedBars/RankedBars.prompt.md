A ranked list for "who": the top five as single cobalt bars, and the rest as one quiet total.

```jsx
<RankedBars items={agents.map((a) => ({ label: a.name, value: a.spend30d }))} top={5} />
```

- One colour. The rank carries the meaning; don't give each row an identity colour.
- Bars scale to the largest of the top N. The catch-all row sits under a hairline in ink-muted with no bar, so it is never the heaviest mark.
- Keep every item reachable in a folded table beside it ("By agent").
