One goal (a top-level assignment) on Home: title, owner, progress from its parts, what is blocked and on whom, and who is working under it.

```jsx
<GoalRow title="Move shift reminders to the new mail provider" owner={{ name: 'Engineering lead', role: 'code', color: 'clay' }}
  done={7} total={12} blocker="Waiting on #42, Test runner" workers={[…]} />
```

- `maxWorkers` is 3 on desktop and 2 on phone; the rest collapse to "+N more" with their names in the tooltip.
- Say the blocker in words ("Waiting on #42, Test runner", "On hold by you"). Never colour alone.
- Put rows in one raised container, hairline-divided. Not cards.
