An outline icon from the Kivali set: a curated subset of Lucide (ISC license), drawn at a 1.5px stroke in `currentColor`.

```jsx
<Icon name="inbox" size={20} />
```

- `name` is one of the set shown here (kebab-case, as Lucide names them); `size` is 16 (inline with text, the default), 20 (buttons and nav) or 24 (empty states and headers). Keep the 1.5px stroke.
- Decorative by default (hidden from screen readers). Pass `label` when the icon is the only thing carrying meaning, such as an icon-only button.
- The icon takes the text color around it; don't color icons on their own except for status (`danger`, `success`, `signal-ink`), and then always beside a word.
- Need an icon that isn't here? Add it from Lucide to `components/src/icons.ts` rather than drawing one; keep the set small.
