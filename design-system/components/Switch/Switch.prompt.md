An on/off setting that takes effect immediately (enabling a skill, auto-release). Built on Radix Switch.

```jsx
<Switch label="Auto-release messages" defaultChecked />
```

- On, the track is `ink` and the thumb turns `signal` yellow: yellow on ink, as the brand rule asks.
- Pass `checked` and `onCheckedChange`; `label` is required and `hint` is optional.
- If the change needs a save button, use `Checkbox` instead.
