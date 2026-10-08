A labeled native select, styled to match `TextField`.

```jsx
<Select label="Reports to" options={[{ value: 'cos', label: 'Chief of Staff' }]} />
```

- `options` is a list of `{value, label}`; `label`, `hint` and `error` work as in `TextField`.
- Use it for short fixed lists (model, effort, resolution). For more than about 12 options or search, a combobox will come later.
