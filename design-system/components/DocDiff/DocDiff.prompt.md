Shows a markdown document (a role, the handbook) as collapsible sections, as a before and after diff or as a plain read.

```jsx
<DocDiff before={currentRole} after={proposedRole} />
<DocDiff after={proposedRole} />   // a new document, e.g. a hire
```

- Each `##` heading becomes a card. Text before the first heading becomes an "Opening" card.
- Added lines sit on `cobalt-soft` with "+"; removed lines are struck through in ink-muted with "−". No red and no green: nothing here has failed.
- Changed sections open; unchanged ones are closed with "No changes" in their meta.
- When a proposal carries two documents (a role and an initial memory), give each its own heading block above its cards.
- The line diff here is deliberately simple (set-based, per section). Swap in a real LCS diff if edits reorder lines.
