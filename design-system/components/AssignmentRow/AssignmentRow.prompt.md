One assignment in a list or tree: the unit of the Work view.

```jsx
<AssignmentRow id={14} title="Plant out the tomato seedlings" state="blocked" waitingOn={[{ id: 12, state: 'ready' }]} />
```

- `id`, `title`, `state` (the `AssignmentState` circle), `assignee` (an agent's `{name, role, color}` for its avatar, or `{kind: "person", name}` for a person), `openChildren` (open child count) and `acceptance` (`{satisfied, claimed, unclaimed}` for a compact `AcceptanceMeter`).
- Trees: `depth` indents 20px per level with elbow lines; `last` ends the line at the last child; `onToggle` and `expanded` add the disclosure chevron.
- Why it can't move is said in the row: `waitingOn` lists the assignments it waits on as `AssignmentRef`s, and `heldBy` says who paused it, both in `signal-ink`. Done and dropped rows go quiet (dropped is struck through).
- `onClick` or `href` makes the row a target; `selected` tints it `cobalt-soft` for the assignment shown in a detail pane.
- `waitingOn[].title`: shown after each ref; when any entry has one, entries are separated by semicolons.
