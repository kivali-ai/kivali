// Lint-the-linter: every rule fires on fixtures/bad at the exact line that breaks it, nothing
// fires on fixtures/good, and a check that crashes fails the run.
import { spawnSync } from "node:child_process";
import path from "node:path";
import { describe, expect, it } from "vitest";

const WEB = path.resolve(path.dirname(new URL(import.meta.url).pathname), "..");

function run(arg) {
  const r = spawnSync("node", ["lint/run.mjs", arg], { cwd: WEB, encoding: "utf8" });
  return { code: r.status, out: r.stdout + r.stderr };
}

// [file:line under lint/fixtures/bad, rule id (and message fragment where one rule covers several cases)]
// Grouped by convention.
const EXPECTED = [
  // CSS: every value is a token. No hex, rgb, hsl. No raw px except 0 and 1px.
  ["bad.css:21", "stylelint/color-no-hex"],
  ["bad.css:21", "token-check/raw-color"],
  ["bad.css:22", "declaration-strict-value] Use a design-system token: var(--…) for background (got red)"],
  ["bad.css:23", "declaration-strict-value] Use a design-system token: var(--…) for padding (got 12px)"],
  ["bad.css:24", "declaration-strict-value] Use a design-system token: var(--…) for margin (got 8px)"],
  ["bad.css:25", "declaration-strict-value] Use a design-system token: var(--…) for border-radius"],
  ["bad.css:26", "declaration-strict-value] Use a design-system token: var(--…) for z-index"],
  ["bad.css:27", "declaration-strict-value] Use a design-system token: var(--…) for font-size"],
  ["bad.css:28", "stylelint/declaration-property-value-disallowed-list"], // raw duration
  ["bad.css:29", "declaration-strict-value] Use a design-system token: var(--…) for font "], // font shorthand
  ["bad.css:30", "declaration-strict-value] Use a design-system token: var(--…) for width"],
  ["bad.css:31", "declaration-strict-value] Use a design-system token: var(--…) for box-shadow"],
  ["bad.css:31", "token-check/raw-color"], // rgba
  ["bad.css:33", "stylelint/declaration-property-value-disallowed-list"], // raw easing
  ["bad.css:34", "declaration-strict-value] Use a design-system token: var(--…) for margin-top (got calc(100% - 12px))"],
  ["bad.css:36", "declaration-strict-value] Use a design-system token: var(--…) for margin-bottom (got -2px)"], // only -1px tucks
  ["bad.css:45", "declaration-strict-value] Use a design-system token: var(--…) for max-height (got calc(50vh - var(--space-4)))"], // 100vh/100dvh only inside calc
  ["bad-tokens.css:9", "token-check/raw-color] Raw color in border-color: rgb("],
  ["bad-tokens.css:10", "token-check/raw-color] Raw color in fill: hsl("],
  ["bad-tokens.css:11", "token-check/raw-color] Raw color in outline-color: #0af"],
  ["bad-tokens.css:7", "token-check/unknown-property] var(--ink-brand)"],
  ["bad-tokens.css:8", "token-check/unknown-property] var(--kv-not-a-thing)"],
  // CSS: no !important.
  ["bad.css:32", "stylelint/declaration-no-important"],
  // CSS: no fonts beyond Space Grotesk, IBM Plex Sans, IBM Plex Mono.
  ["bad.css:3", "stylelint/at-rule-disallowed-list"],
  ["bad.css:4", "token-check/font-family"],
  ["bad.css:35", "declaration-strict-value] Use a design-system token: var(--…) for font-family"],
  ["bad-tokens.css:12", "token-check/font-family"],
  // CSS: app classes are prefixed app- and live in src/styles/; never restyle a kv- class.
  ["bad.css:12", "stylelint/selector-class-pattern] Class .plain-class"],
  ["bad.css:8", "stylelint/selector-disallowed-list"], // .kv-button
  ["bad.css:16", "stylelint/selector-disallowed-list"], // bare .is-open
  ["bad.css:1", "token-check/css-location"],
  ["bad-tokens.css:1", "token-check/css-location"],
  // CSS under src/ds (Kivali additions such as text.css): .kv-* classes allowed, token rules still apply.
  ["src/ds/text.css:3", "declaration-strict-value] Use a design-system token: var(--…) for font "], // font: 500 20px/28px …
  ["src/ds/text.css:4", "declaration-strict-value] Use a design-system token: var(--…) for padding"],
  ["src/ds/text.css:5", "token-check/unknown-property] var(--kv-text-nope)"],
  ["src/ds/text.css:8", "stylelint/selector-disallowed-list] Design-system CSS never styles app- classes"],
  ["src/ds/text.css:12", "stylelint/selector-class-pattern] Class .Text_Heading"],
  // CSS: @import only design-system files; custom properties the app defines are --app-*.
  ["bad-tokens.css:2", 'token-check/css-import] @import "./other.css"'],
  ["bad-tokens.css:3", 'token-check/css-import] @import "https://fonts.googleapis.com'],
  ["bad-tokens.css:6", "token-check/app-definition] --brand"],

  // TSX: design-system components only from the src/ds barrel; Radix, icon libs and kits banned.
  ["Bad.tsx:3", "'@radix-ui/react-dialog' import is restricted"],
  ["Bad.tsx:4", "'lucide-react' import is restricted"],
  ["Bad.tsx:5", "'styled-components' import is restricted"],
  ["Bad.tsx:6", "'@emotion/react' import is restricted"],
  ["Bad.tsx:7", "'@mui/material/Button' import is restricted"],
  ["Bad.tsx:8", "src/ds/Badge/Badge' import is restricted"],
  // TSX: no raw button, input, select, textarea, table, dialog, h1..h6 (and a, and inline svg).
  ["Bad.tsx:24", "<button> is forbidden"],
  ["Bad.tsx:25", "<input> is forbidden"],
  ["Bad.tsx:26", "<select> is forbidden"],
  ["Bad.tsx:27", "<textarea> is forbidden"],
  ["Bad.tsx:28", "<table> is forbidden"],
  ["Bad.tsx:29", "<dialog> is forbidden"],
  ["Bad.tsx:30", "<h1> is forbidden"],
  ["Bad.tsx:31", "<h2> is forbidden"],
  ["Bad.tsx:32", "<h3> is forbidden"],
  ["Bad.tsx:33", "<h4> is forbidden"],
  ["Bad.tsx:34", "<h5> is forbidden"],
  ["Bad.tsx:35", "<h6> is forbidden"],
  ["Bad.tsx:36", "<a> is forbidden"],
  ["Bad.tsx:37", "<svg> is forbidden"],
  // TSX: no style= except a documented --kv-* variable passed to a component.
  ["Bad.tsx:23", "oxlint/react(forbid-dom-props)"],
  ["Bad.tsx:40", "syntax/style"],
  // TSX: the designer's no-restricted-syntax rules (hex, px, font, prop enums, unknown props).
  ["Bad.tsx:12", "syntax/literal] Raw hex color"],
  ["Bad.tsx:13", "syntax/literal] Raw px value"],
  ["Bad.tsx:14", "syntax/literal] Font not provided"],
  ["Bad.tsx:15", "syntax/literal] Raw px value"], // a media query outside src/ds/breakpoints.ts
  ["Bad.tsx:38", "syntax/enum] <Button> variant must be one of"],
  ["Bad.tsx:39", "syntax/props] <Button> doesn't accept that prop"],
  ["bad-values.ts:2", "syntax/literal] Raw hex color"], // plain .ts is linted
  ["bad-values.ts:3", "syntax/literal] Raw px value"],
  ["src/ds/Leaky/Leaky.tsx:4", "syntax/literal] Raw px value"], // src/ds is not blanket-exempt
  ["src/ds/Leaky/breakpoints.ts:2", "syntax/literal] Raw px value"], // the exemption is one exact path
  // TSX: token rules over strings and object keys.
  ["Bad.tsx:12", "token-check/raw-color"],
  ["BadCopy.tsx:5", "token-check/raw-color"],
  ["BadCopy.tsx:6", "token-check/raw-color] Raw color in string \"background: rgb("],
  ["BadCopy.tsx:7", "token-check/unknown-property"],
  ["BadCopy.tsx:8", "token-check/app-definition"],
  ["BadCopy.tsx:9", "token-check/font-family"],

  // Copy: no banned words, exclamation marks or emoji; sentence case labels.
  ["BadCopy.tsx:13", 'copy/banned-word] Banned word "Simply"'],
  ["BadCopy.tsx:14", 'copy/banned-word] Banned word "seamless"'],
  ["BadCopy.tsx:15", 'copy/banned-word] Banned word "bots"'],
  ["BadCopy.tsx:16", 'copy/banned-word] Banned word "magic"'],
  ["BadCopy.tsx:17", 'copy/banned-word] Banned word "AI-powered"'],
  ["BadCopy.tsx:18", "copy/exclamation"],
  ["BadCopy.tsx:19", "copy/emoji"],
  ["BadCopy.tsx:20", 'copy/title-case] Title Case in a button label ("Hire New Agent"'],
  ["BadCopy.tsx:21", 'copy/title-case] Title Case in title= ("What It Remembers"'],
  ["BadCopy.tsx:22", 'copy/title-case] Title Case in label= ("Release All"'],

  // Escape hatch: a directive covers only the rule it names, must name a known rule, and must be used.
  ["Bad.tsx:17", "syntax/literal] Raw px value"],
  ["Bad.tsx:16", "kivali-lint/directive] Unused kivali-lint-disable-next-line copy/emoji"],
  ["Bad.tsx:18", 'kivali-lint/directive] Unknown rule "syntax/litteral"'],
  ["Bad.tsx:20", "kivali-lint/directive] kivali-lint-disable-next-line needs a rule list"],
  ["bad.css:39", "kivali-lint/directive] Unused kivali-lint-disable-next-line token-check/raw-color"],
];

describe("design-system lint", () => {
  const bad = run("--fixtures=bad");
  const lines = bad.out.split("\n");

  it("fails on fixtures/bad", () => {
    expect(bad.code).toBe(1);
    expect(bad.out).toContain("fixtures/bad: FAIL");
  });

  it.each(EXPECTED)("reports %s %s", (loc, fragment) => {
    const hit = lines.some((l) => l.includes(`lint/fixtures/bad/${loc} [`) && l.includes(fragment));
    expect(hit, `expected a line for ${loc} containing ${fragment}`).toBe(true);
  });

  it("does not flag allowed constructs in fixtures/bad", () => {
    // BadCopy.tsx line 10 passes a documented --kv-* variable; line 23 is an allowlisted proper noun.
    expect(bad.out).not.toContain("BadCopy.tsx:10 ");
    expect(bad.out).not.toContain("BadCopy.tsx:23 ");
  });

  it("passes on fixtures/good (breakpoints file, @media px, token calc, used directives, ds css imports)", () => {
    const good = run("--fixtures=good");
    expect(good.out).toContain("fixtures/good: PASS");
    expect(good.code).toBe(0);
  });

  it("fails, naming the file and line, when a file does not parse", () => {
    const crash = run("--fixtures=crash");
    expect(crash.code).toBe(1);
    expect(crash.out).toContain("fixtures/crash: FAIL");
    expect(crash.out).toMatch(/lint\/fixtures\/crash\/Broken\.tsx:[1-9]\d* \[syntax-check\/crash\] parse error/);
    expect(crash.out).toMatch(/lint\/fixtures\/crash\/Broken\.tsx:[1-9]\d* \[oxlint\/parse-error\]/);
  });
});
