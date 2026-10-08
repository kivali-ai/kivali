A reference to an assignment inside text or a row: `#42`, optionally with its state and title.

```jsx
<AssignmentRef id={12} title="Order seeds" state="ready" />
```

- `id` is required; `state` adds the `AssignmentState` circle so a reader sees at a glance whether the referenced work is done; `title` adds the name (truncated; the full title shows on hover); `href` makes it a link.
- Use it wherever assignments mention each other: "waiting on", parents, acceptance items, messages from agents.
