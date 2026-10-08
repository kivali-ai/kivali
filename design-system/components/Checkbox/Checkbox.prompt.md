A checkbox with its label, built on Radix Checkbox for keyboard and screen-reader support.

```jsx
<Checkbox label="Include past chats" defaultChecked />
```

- Use it for choices that apply when a form is submitted, and in table rows for bulk selection. For settings that take effect immediately, use `Switch`.
- Pass `checked` and `onCheckedChange`, or `defaultChecked`. `checked="indeterminate"` shows a dash (a partial "select all").
- `label` is required; `hint` adds a muted second line.
- `ariaLabel`: accessible name when `label` is empty (a row-selection checkbox). `name` passes through.
