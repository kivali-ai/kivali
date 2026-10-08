Progress on an assignment's acceptance items: how many named deliverables are met, in progress, or unclaimed.

```jsx
<AcceptanceMeter satisfied={3} claimed={1} unclaimed={1} />
```

- `satisfied` fill `success`, `claimed` fill `cobalt` (an open child is working on it), `unclaimed` are outlined in `signal-ink`: work nobody has noticed, the thing to catch.
- The text reads "3/5 met · 1 in progress · 1 unclaimed". `compact` shows the bar alone (56px) for rows, with the text as its accessible name.
- Assignments without acceptance items show their open-children count instead; never fake a meter from child counts.
