Text in the Kivali type scale: use it for headings, captions and labels on screens instead of hand-set font styles.

```jsx
<Text as="h1" variant="title">Garden advisor</Text>
<Text as="p" variant="caption" tone="muted">Checks the forecast each morning.</Text>
<Text variant="label">LAST RUN 06:00</Text>
```

- `variant`: `display` (the one big number or name on a page), `title` (page title), `heading` (section), `body` (default), `caption` (secondary lines), `label` (mono, small metadata), `code` (mono).
- `tone`: `default` (ink), `muted`, `faint`, `signal` (needs the person), `cobalt`, `success`, `danger`. Omit to inherit.
- `as` picks the element for meaning; margins are zeroed.
