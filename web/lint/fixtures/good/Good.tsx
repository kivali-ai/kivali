// Fixture: clean app code. Design system through the barrel, tokens in CSS.
import "../ds/index.css";
import { Link } from "react-router";
import { AutoRelease, Badge, Button, Icon, TextField } from "../ds";

export function Good({ name, frac }: { name: string; frac: number }) {
  // kivali-lint-disable-next-line syntax/literal -- a third-party widget API that takes a px string
  const legacyOffset = "2px";
  return (
    <div className="app-card is-open" data-offset={legacyOffset}>
      <div className="kv-title">Team</div>
      <TextField label="Search" placeholder="Find an agent" />
      <Badge tone="neutral">{name}</Badge>
      <Button variant="primary" onClick={() => undefined} aria-label="Hire agent">
        <Icon name="plus" />
        Hire agent
      </Button>
      <AutoRelease value={frac} style={{ "--kv-autorel-frac": frac }} />
      {/* kivali-lint-disable-next-line copy/exclamation -- quoting a user's message verbatim */}
      <p>Ship it!</p>
      <Link to="/team">See the team</Link>
      {/* eslint-disable-next-line react/forbid-elements -- external */}
      <a href="https://example.com">Docs</a>
    </div>
  );
}
