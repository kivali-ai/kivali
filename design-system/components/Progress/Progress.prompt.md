A thin bar for something measurable: an upload, budget used, setup steps.

```jsx
<Progress value={62} label="Indexing files" />
```

- `value` and `max` (default 100), a `label` shown above with the percentage, and `tone`: `ink` (default), `cobalt`, `success`.
- For acceptance items on an assignment, use `AcceptanceMeter`, which shows satisfied, claimed and unclaimed separately.
- For work with no measurable end, use the button's loading dots or `AgentState`, not an endless bar.
