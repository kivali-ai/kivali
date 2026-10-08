Switches between views of one thing (an agent's chat, memory, role and past chats). Built on Radix Tabs: arrow keys move between tabs.

```jsx
<Tabs tabs={[{ value: 'chat', label: 'Chat' }, { value: 'files', label: 'Files', count: 12 }]} />
```

- `tabs` is a list of `{value, label, count?, content?}`; `count` shows a small neutral number. Control with `value` and `onValueChange`, or let it manage itself.
- The active tab is `ink` with a 2px underline; the rest are `ink-muted`. Keep labels to one or two words, sentence case.
- Tabs switch views of the same object. For moving between different places, use `NavItem`.
