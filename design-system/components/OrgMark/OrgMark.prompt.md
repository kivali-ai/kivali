An org's mark: its uploaded logo, or, until one is uploaded, its initials on its identity color.

```jsx
<OrgMark name="Sunset Gardens" color="pine" size={28} />
```

- `name`, `src` (the uploaded logo) and `color` (the org's identity color, which defaults to the palette color closest to the logo's main color). Sizes 20 (menus), 28 (the switcher) and 40 (settings, headers).
- Orgs are rounded squares with a slightly tighter corner than agents, and always carry their own name next to them in lists.
- The org's brand lives here and in the org header only. It never recolors Kivali's chrome, buttons or status colors.
- The logo is a square mark (the same square PNG or SVG the app already asks for as the favicon), shown whole inside the tile with the tile's corners. Wide wordmarks don't fit; ask for the square symbol instead.
- Where an org's logo appears: the `OrgSwitcher` (the current org at 28px and each org in the menu at 20px), the org's settings and profile header at 40px, the browser tab and home-screen icon for that org, and its sign-in page. It does not appear in Kivali's top bar, on buttons, or inside chats: those stay Kivali's.
- On upload, the org's identity color is set to the palette color closest to the logo's main color; the person can change it. That color is used where the logo is too small or absent, such as charts comparing orgs.
