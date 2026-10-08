A labeled text input, or a text area with `multiline`.

```jsx
<TextField label="Agent name" hint="Shown in chat and on the org chart." placeholder="Garden advisor" />
```

- Always pass `label`; add `hint` for one line of guidance, or `error` to show a `danger` message with an icon (it replaces the hint and sets `aria-invalid`).
- `multiline` renders a resizable `<textarea>` with `rows` (default 4). Use it for briefs, replies and assignment descriptions.
- Placeholders are a real example of valid input, never a repeat of the label.
- Other native input props (`name`, `value`, `onChange`, `required`) pass through.
