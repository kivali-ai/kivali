The top of the sidebar: shows the current org and switches between the orgs the person belongs to. Built on Radix Dropdown Menu.

```jsx
<OrgSwitcher current="sunset" orgs={[{ id: 'sunset', name: 'Sunset Gardens', color: 'pine' }]} />
```

- `orgs` is a list of `{id, name, src?, color}`; `current` is the active id; `onSelect(id)` and `onCreate()` handle the menu.
- Each org shows its `OrgMark` and name; the current one carries a check. "New org" sits last, after a separator.
- Switching orgs changes the org mark, name and data; Kivali's chrome stays the same.
