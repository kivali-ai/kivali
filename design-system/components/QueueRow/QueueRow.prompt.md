One message waiting in the queue between agents.

```jsx
<QueueRow from={engLead} to={[testRunner]} title="Mail provider trial extended to Friday" releasesIn={22} onRelease={release} />
```

- Release is one step, secondary. The Queue's one primary stays "Release all" (or "Release N selected").
- The countdown is mono and fixed when the message lands. It reads "Held" when auto-release is Off.
- `selectable` shows the checkbox; use it only when auto-release is Off.
- Expanded children hold the body, a note field ("Delivered as top-priority direction from you."), Raw message, Bounce (notices only, comment required) and "Release with note".
