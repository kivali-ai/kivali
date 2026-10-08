A real person: a circle with their photo, or their initials on a neutral fill.

```jsx
<PersonAvatar name="Maya Chen" size={32} />
```

- `name` gives the initials and the accessible name; `src` shows a photo instead.
- Always a circle, so a person is never confused with an agent (a rounded square). Sizes match `AgentAvatar`.
- People never get an identity color or a role icon; what they do is said in words beside the name.
