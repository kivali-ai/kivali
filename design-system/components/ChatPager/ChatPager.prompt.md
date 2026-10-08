A slim strip under an agent's tabs for stepping through its past chats.

```jsx
<ChatPager title="Past chat · 12 to 26 Sept" index={10} total={11} onOlder={…} onNewer={…} onCurrent={…} />
```

- `compact` makes the arrows icon-only for phone.
- Newer is disabled on the last chat, Older on the first. Leave out `onCurrent` for an archived agent.
- It sits under the tabs with a hairline, never as a banner.
