# Kivali app UI kit

An interactive composition of the Kivali product shell: org sidebar, inbox, agent chat, work, team and knowledge views, plus the hire dialog and a toast.

**Disclaimer:** the source kit defines components and patterns only ("page layouts are designed with it, in Kivali's page designs, not here"). No product screens were provided, so these views are assembled strictly from the documented rules (sidebar + one content column, `content` / `content-wide` widths, one primary per view, cards for decisions and rows for everything else, chat as a transcript). Replace with real page designs when available.

- `index.html` — app shell and routing; hire dialog; toast. Last view persists in localStorage.
- `Sidebar.jsx` — OrgSwitcher, NavItems, agent list, Hire agent.
- `ChatScreen.jsx` — transcript with Thinking, ToolCall, SubagentGroup, Composer; sending a message fakes an agent reply.
- `WorkScreens.jsx` — InboxScreen, WorkScreen, TeamScreen, GraphScreen.
