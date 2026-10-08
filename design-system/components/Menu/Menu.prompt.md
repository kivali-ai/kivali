A short list of actions behind a button (usually an icon-only `ghost` button with `ellipsis`). Built on Radix Dropdown Menu: keyboard, typeahead and focus are handled.

```jsx
<Menu trigger={<Button variant="ghost" iconOnly icon={<Icon name="ellipsis" />} aria-label="Actions" />} items={[{ icon: 'pencil', label: 'Edit brief' }, { separator: true }, { icon: 'archive', label: 'Offboard', danger: true }]} />
```

- `trigger` is the element that opens it; `items` are `{label, icon?, shortcut?, danger?, disabled?, onSelect}` or `{separator: true}`.
- Destructive items are `danger`, sit last after a separator, and open a confirm `Dialog` rather than acting at once.
- Keep menus under about eight items; group with separators, not headings.
- `side`: which side of the trigger the menu opens on (`bottom` default; the sidebar account menu opens `top`). Items accept `right` for a trailing node such as a check on the current choice.
