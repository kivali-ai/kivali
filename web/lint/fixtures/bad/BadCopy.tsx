// Fixture: violates every copy-lint and TSX token-check rule at least once. Not part of the app.
import { Button, Card } from "../ds";

export function BadCopy() {
  const swatch = "color: #ff8800"; // rule: token-check/raw-color (hex in TSX)
  const other = "background: rgb(1, 2, 3)"; // rule: token-check/raw-color (rgb in TSX)
  const css = "var(--not-a-token)"; // rule: token-check/unknown-property (TSX)
  const def = { "--brand": "1" }; // rule: token-check/app-definition (TSX)
  const face = "font-family: Georgia, serif"; // rule: token-check/font-family (TSX)
  const fine = { "--kv-autorel-frac": 0.4 }; // allowed: a documented --kv-* variable
  return (
    <div>
      <p>Simply connect your account.</p> {/* rule: copy/banned-word (simply) */}
      <p>A seamless handoff.</p> {/* rule: copy/banned-word (seamless) */}
      <p>Your bots are ready.</p> {/* rule: copy/banned-word (bots) */}
      <p>It works like magic.</p> {/* rule: copy/banned-word (magic) */}
      <p>Our AI-powered inbox.</p> {/* rule: copy/banned-word (AI-powered) */}
      <p>Welcome back!</p> {/* rule: copy/exclamation */}
      <p>Shipped it 🎉</p> {/* rule: copy/emoji */}
      <Button>Hire New Agent</Button> {/* rule: copy/title-case (Button children) */}
      <Card title="What It Remembers" /> {/* rule: copy/title-case (title=) */}
      <Card label="Release All" /> {/* rule: copy/title-case (label=) */}
      <Button>Ask Chief of Staff</Button> {/* allowed: proper noun in the allowlist, but Ask + Chief of Staff is fine */}
      {swatch}
      {other}
      {css}
      {def.x}
      {face}
      {fine.x}
    </div>
  );
}
