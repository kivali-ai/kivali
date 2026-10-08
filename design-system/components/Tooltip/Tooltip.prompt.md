A short label that appears on hover or focus, mainly to name icon-only buttons. Built on Radix Tooltip.

```jsx
<Tooltip content="Release all queued messages"><Button iconOnly icon={<Icon name="send" />} aria-label="Release all" /></Tooltip>
```

- `content` is a few words in sentence case, no period. `side` places it; it flips when there's no room.
- Tooltips repeat or clarify; they never hold information found nowhere else, because touch screens can't hover.
- `ink` fill with `on-ink` text, in both themes.
