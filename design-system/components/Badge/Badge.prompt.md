A small pill for a count, a kind, a model or a short status word.

```jsx
<Badge tone="cobalt">Learned</Badge>
```

- `tone`: `neutral` (default), `signal` (needs the person), `cobalt` (learned, in progress), `success`, `danger`.
- `variant`: `soft` (default, tinted), `solid` (for counts and the one thing to notice), `outline` (quiet metadata).
- `mono` sets it in the mono face, for models, IDs and readouts.
- Keep the text to one to three words. For an agent's run state use `AgentState`, not a badge.

## When to use a badge

Use a badge when the status is something to act on or scan for; otherwise say it in plain text.

- **Badge:** the status is why the item is in front of the person (Needs your approval), or it's what separates an item from its neighbors in a list with mixed statuses (one Failed among many Done).
- **Plain text in the meta line:** the status is settled history, or every item around it shares it ("Done yesterday" in a list of finished work). Put the time with it; the time is usually what matters.
- **Neither:** the page or section already says it (a "Done" section needs no Done badge on each item).
- At most two badges per item: one status, plus one piece of metadata if it really earns the space.

## Which tone for which meaning

The system fixes the meanings; each screen picks its own words.

| Meaning | Tone | Example words |
| --- | --- | --- |
| Needs the person to act | `signal` | Needs your approval · Needs a reply · Needs you |
| In progress, or learned something | `cobalt` | Queued · In review · Learned 3 things |
| Finished well | `success` | Approved · Done · Restored |
| Failed or broken | `danger` | Failed · Couldn't send · Expired |
| Settled, neutral, or no longer active | `neutral` | Denied · Closed · Cancelled · Draft · Archived |
| Metadata, not status | `neutral`, `outline`, often `mono` | opus · high · #142 · 3 files |

- A decision the person made is never `danger`: "Denied" is neutral, because nothing went wrong.
- Only one `signal` badge per card, and it is the reason the card is in front of the person.
- If a new status doesn't fit a row, it probably belongs to the closest one; add a tone only for a genuinely new meaning.
