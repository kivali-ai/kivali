// index.html#/<route>: which page a window shows (docs/developers/desktop-app.md, "The bundled pages").

export type SetupMode = "welcome" | "add" | "new";
export type TeamTab = "overview" | "ai" | "mac" | "devices";

export type Route =
  | { page: "setup"; mode: SetupMode }
  | { page: "connect" }
  | { page: "settings"; section: "general" | "advanced" }
  | { page: "settings"; section: "team"; id: string; tab: TeamTab }
  | { page: "team"; id: string };

const TABS: TeamTab[] = ["overview", "ai", "mac", "devices"];

/** Anything unknown falls back to the welcome screen. */
export function parseRoute(hash: string): Route {
  const parts = hash
    .replace(/^#\/?/, "")
    .split(/[?]/)[0]
    .split("/")
    .filter(Boolean)
    .map((p) => decodeURIComponent(p));
  const [a, b, c, d] = parts;
  if (a === "setup") {
    if (b === "add" || b === "new") return { page: "setup", mode: b };
    return { page: "setup", mode: "welcome" };
  }
  if (a === "connect") return { page: "connect" };
  if (a === "settings") {
    if (b === "advanced") return { page: "settings", section: "advanced" };
    if (b === "team" && c) {
      const tab = (TABS as string[]).includes(d) ? (d as TeamTab) : "overview";
      return { page: "settings", section: "team", id: c, tab };
    }
    return { page: "settings", section: "general" };
  }
  if (a === "team" && b) return { page: "team", id: b };
  return { page: "setup", mode: "welcome" };
}

export function routeHash(r: Route): string {
  switch (r.page) {
    case "setup":
      return `#/setup/${r.mode}`;
    case "connect":
      return "#/connect";
    case "team":
      return `#/team/${encodeURIComponent(r.id)}`;
    case "settings":
      return r.section === "team"
        ? `#/settings/team/${encodeURIComponent(r.id)}/${r.tab}`
        : `#/settings/${r.section}`;
  }
}

/** The setup window's title for each of its routes (set_title). */
export function setupTitle(r: Route): string | null {
  if (r.page === "connect") return "Connect to a team";
  if (r.page !== "setup") return null;
  return { welcome: "Welcome to Kivali", add: "Add a team", new: "New team" }[r.mode];
}
