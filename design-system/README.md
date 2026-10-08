# Kivali Design System

Kivali is an open-source, persona-centric agentic platform: you build a team of AI agents that sit in an org structure, each with a persona, tools and memory, that learn your world and adapt. It serves founders running a company and individuals running their lives (a gardener that knows your garden, an advisor that knows your priorities).

The brand direction is **Workshop**: a well-made instrument. Light, warm and precise, with honey yellow (`signal`) as the rare signal and a dusty cobalt as the working accent. Slick, simple, powerful, friendly to pick up; never cold, bulky or toy-like.

## Products

The **Kivali web app** (org sidebar + one content column; inbox, chat with agents, assignments, team/org chart, knowledge graph) and **Kivali Desktop**'s own pages (setup, Settings), which use the same CSS, tokens, fonts and logos. The source defines components and patterns only, not page layouts.

---

## CONTENT FUNDAMENTALS

- **Voice:** plain, warm and confident, like a good colleague. Short sentences. Say what it does. A little humor is allowed when the moment is small (empty states, success notes), never in errors.
- **Casing:** sentence case everywhere: buttons, titles, labels, menu items ("Hire agent", "What it remembers", "Release all"). Mono labels are sentence case too, lightly tracked ("Memory · 214 notes").
- **Punctuation:** no exclamation marks. The middle dot `·` separates readout parts. No emoji.
- **Banned words:** "simply", "seamless", "bots", "magic", "AI-powered".
- **Agents are colleagues:** "hire", "brief", "offboard", "your team", "what it remembers", "reports to". Agents "learn" things; they "need help" rather than "error".
- **Person:** the product addresses the person as "you"; agents speak in first person ("I'll check again at 6pm").
- **Blockers in words:** say why something can't move ("Waiting on #12", "On hold by Maya"), never only a color.
- **Name:** Kivali (said kih-VAH-lee), capitalized in running text; the wordmark is lowercase and never retyped in live text.
- **Examples:** "Hire your first agent." · "Your gardener learned 3 things this week." · "No agents yet. Every team starts with one." · "Couldn't reach the weather service. Retrying in 5 minutes." · "Offboard Garden advisor? Its files move to the archive. Nothing is deleted, and you can restore it later." · Tagline: "The agentic OS with personality."

## VISUAL FOUNDATIONS

- **Color:** page on warm `paper` (#F6F3EA, never white); cards, panels, inputs on `paper-raised`; `line` hairlines divide. Text is `ink` (warm near-black, never pure black) and `ink-muted`; `ink-faint` only for control borders, inactive icons and the dot grid. `signal` honey is rare: at most one highlight per view, always on or beside ink, never thin text/lines on paper in light theme; text on it is `on-signal` ink. `cobalt` does the work: links, selection, focus, "learned". Status: `success` teal (off the red-green axis), `danger`, `signal-ink` for attention. Identity colors (clay…slate) mean *who*, never status. Dark theme via `[data-theme="dark"]` keeps the warmth.
- **Type:** Space Grotesk (display, title, heading, wordmark; never below 20px except card titles at 17px in source), IBM Plex Sans (body 15/24, caption 13/18), IBM Plex Mono (label 11/16 +0.02em, code 13/20). One `display` per page. Tight negative tracking on display sizes.
- **Spacing:** 4px base; `space-2` inside controls, `space-4` card padding and grid gaps, `space-6` between groups, `space-8` between sections and page gutters.
- **Corners:** soft. 6px checkboxes/badges/tooltips, 10px buttons/inputs/menus, 16px cards/panels/agent tiles, full for status chips, avatars, signal dot. Agent avatars round at 28% of size, org marks 24%. Round accents (discs, pills) welcome; no blobby shapes.
- **Cards:** `paper-raised`, 1px `line` border, 16px radius, `shadow-sm` (0 1px 2px ink 8%). Attention cards swap to a 1.5px `ink` border. Lists of rows sit in one raised container; rows are hairline-divided, not individual cards.
- **Depth:** borders first, then `shadow-sm` at rest, `shadow-md` for menus/popovers/tooltips/toasts, `shadow-lg` for dialogs only. Stacking only via `z-*` tokens.
- **Backgrounds & texture:** flat paper. The brand texture is a dot grid at 16px pitch in `ink-faint`, used sparingly (empty states, marketing), with signal discs and cobalt pills as friendly counterparts. No photography, no gradients (the only gradient is the skeleton shimmer), no illustrations beyond the logo motif.
- **Motion:** `duration-fast` 120ms hovers/presses, `duration-base` 200ms switches/disclosures/menus, `duration-slow` 320ms dialogs/toasts; `ease-out` cubic-bezier(0.2,0.7,0.2,1) default, `ease-in-out` for the switch thumb. Dialogs fade + rise 8px. Nothing bounces except AgentState's running dots (and the button loading dots). Everything stops under reduced motion except AgentState's running dots, which fade slowly in place so a running agent never reads as stopped.
- **Hover:** primary darkens (ink mixed 86% with paper); secondary border goes `ink-faint` → `ink`; ghost fill deepens; rows tint with half-strength `line`; nav items get a `line` fill.
- **Press:** buttons translate down 1px. No scale.
- **Focus:** `focus-ring` everywhere: a 2px paper gap then 2px solid cobalt.
- **Selection:** `cobalt-soft` row tint; active nav is a raised `paper-raised` pill with line inset + `shadow-sm`; active tab is a 2px ink underline.
- **Transparency & blur:** minimal. Dialog overlay is ink at 40%. No backdrop blur.
- **Layout:** sidebar 264px + one content column (`content-narrow` 640, `content` 880, `content-wide` 1200); below 960 the sidebar becomes a drawer, below 640 one column.
- **Shapes carry meaning:** agents are dots (AgentState) and rounded squares (AgentAvatar), people are circles, assignments are circles (AssignmentState), graph nodes are diamonds/squares/faint circles (NodeChip).
- **Charts:** fixed 8-slot series order (lake, olive, rose, ochre, plum, pine, clay, iris), thin marks with 2px paper gaps, ink text, always a legend, one y-axis. `seq-1..5` for magnitude, clay/lake around `diverging-mid` for polarity.

## ICONOGRAPHY

- Lucide (ISC), outline only, 1.5px stroke on a 24px grid, `currentColor`. Sizes 16, 20, 24 (18 in nav/banners/toasts per source). The curated 60-icon subset is embedded in `components/Icon/Icon.jsx` (`ICONS`, `iconNames`); render via `<Icon name="…" />`. Add missing icons from Lucide to that map.
- Role icons for agent avatars (a separate Lucide subset, drawn at 1.75px) live in `components/AgentAvatar/AgentAvatar.jsx` (`ROLE_ICONS`). The hiring agent picks one for each agent from the app's role icon set, by name and description.
- No filled icons, no emoji, no unicode glyphs as icons (the `·` separator is punctuation), no hand-drawn one-offs. No icon font or sprite.
- Logos: `assets/logos/` (SVG lockup / wordmark / icon in light, dark, mono-ink, mono-paper; PNG icons 32/180/512). Lockup by default; wordmark when the icon is nearby; icon alone for favicons/avatars. Clear space = one honey dot; minimum icon 16px, wordmark 64px wide. Never recolor or redraw.

---

## Index

- `styles.css` — entry point; imports only.
- `tokens/` — `colors.css` (light + `[data-theme="dark"]`, semantic aliases), `typography.css` (@font-face + type scale), `space.css` (spacing, radius, motion, z, layout), `base.css` (page defaults), `tokens.json` (source of truth).
- `components/kivali.css` — all `kv-` component styles (verbatim from source `bundle.css`); `components/text.css` — type classes for the Text component (Kivali's own, not from the export).
- `components/<Name>/` — `<Name>.jsx`, `.d.ts`, `.prompt.md`, `.card.html`.
- `guidelines/` — foundation specimen cards (Colors, Type, Spacing, Brand), `charts.html`, `graphlinks.html` + `graph-links.md`, `cover.html`.
- `fonts/` — Space Grotesk, IBM Plex Sans, IBM Plex Mono (woff2, SIL OFL).
- `assets/logos/`.
- `ui_kits/app/` — interactive Kivali app composition (see its README disclaimer).
- `SKILL.md` — Agent Skills entry.

## Components

- **Core:** Text, Icon, Button, TextField, Select, Checkbox, Switch, Badge, Card, Banner, EmptyState, Dialog (+ DialogClose), Tabs, Menu (+ MenuItem), Tooltip, Toast, ListRow, Table, FileDrop, Progress, Skeleton, Prose, CodeBlock, AutoRelease, Readouts
- **Identity & navigation:** AgentAvatar, PersonAvatar, OrgMark, OrgSwitcher, NavItem, AgentState
- **Chat:** Message, Thinking, ToolCall, SubagentTask, SubagentGroup, Composer, ContextGauge (+ ContextCount), ChatPager
- **Work:** AssignmentState, AssignmentRef, AcceptanceMeter, AssignmentRow, NodeChip, GoalRow, QueueRow, DocDiff (+ DiffLine, splitSections), NodeRow
- **Charts:** RankedBars, DayBars
- Patterns without a component (cards only): Charts, GraphLinks, Cover.

### Deviations from source

- The source builds Checkbox, Switch, Dialog, Tabs, Menu, OrgSwitcher and Tooltip on Radix UI. Here they are dependency-free re-implementations with the same props and `data-state` attributes, rendered in place (no portals; the `container` prop is dropped). In production, keep Radix as the source README specifies.
- `usePopover`, `MenuItem` and `AssignmentGlyph` are exported as small shared helpers.
