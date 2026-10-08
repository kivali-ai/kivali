A single-line graph node that opens to show its fields.

```jsx
<NodeRow node={{ id: 'req/reminder-delay', kind: 'requirement', summary: '…', status: 'flagged', owner: 'clay' }}
  short="flagged by Test runner" fields={[['Path', 'requirements/reminder-delay.md']]} expanded={open} onToggle={toggle} />
```

- `compact` (phone) drops the short fact so the chip never clips.
- Group rows under folded owner sections (Card collapsible, meta "41 nodes · 1 flagged"). Show a few, then "Show all N".
- Put search and the Flagged and Problems filters above the list.
- Uses NodeChip's own flagged, superseded and unresolved looks.
