An agent's face: its initials top-left and its role icon bottom-right, on a rounded-square tile in one of the identity colors.

```jsx
<AgentAvatar name="Garden advisor" role="sprout" color="olive" size={40} />
```

- `name` is the agent's name; initials come from its first two meaningful words ("Chief of Staff" is CS), or the first two letters of a one-word name ("Bookkeeper" is BO). Pass `initials` to override.
- `role` is a role icon name from the set shown here. Without one the tile shows its initials only.
- `color` is one of the identity colors (`clay`, `ochre`, `olive`, `pine`, `lake`, `iris`, `plum`, `rose`, `slate`). Without it, a color is derived from the name so it stays stable.
- `size`: 56 (profile header), 40 (cards, org chart), 32 (chat and lists, the smallest with the role icon), 24, 20, 16 (initials only, dense lists and the tree).
- Agents are always rounded squares and people are always circles (`PersonAvatar`). Never draw an agent as a circle or a person as a square, and never use a robot face for an agent.
- Choosing the role icon: the hiring agent picks it when it proposes the hire, from the icon names and what each shows, with the role in hand. The identity color is hashed from the agent's slug, so it is the same on every screen.
