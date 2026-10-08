Rows and columns for data people compare: files, usage, skills, assignment items.

```jsx
<Table columns={[{ key: 'name', label: 'Agent' }, { key: 'runs', label: 'Runs', align: 'right', mono: true }]} rows={[{ id: 1, name: 'Bookkeeper', runs: 42 }]} />
```

- `columns` are `{key, label, align?, width?, mono?, render?}`; `rows` are objects, keyed by `rowKey` (default `id`). `mono` sets a column in the mono face for numbers, IDs and dates; right-align numbers.
- Headers are small mono labels; the header row sticks while scrolling; rows hover-tint and divide with hairlines. `dense` tightens rows for long lists.
- The table scrolls sideways inside its rounded container on narrow screens rather than squeezing columns.
- If there's only one meaningful column, use `ListRow` instead.
