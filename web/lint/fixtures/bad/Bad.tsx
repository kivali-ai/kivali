// Fixture: violates every oxlint and syntax-check rule at least once. Not part of the app.
// lint.test.mjs asserts each violation by file:line, so keep line numbers stable when editing.
import * as Dialog from "@radix-ui/react-dialog"; // rule: no-restricted-imports (radix outside src/ds)
import { Check } from "lucide-react"; // rule: no-restricted-imports (lucide-react)
import styled from "styled-components"; // rule: no-restricted-imports (styled-components)
import { css } from "@emotion/react"; // rule: no-restricted-imports (@emotion)
import Button2 from "@mui/material/Button"; // rule: no-restricted-imports (@mui)
import { Badge } from "../../../src/ds/Badge/Badge"; // rule: no-restricted-imports (ds internals, use the barrel)
import { Button, Card } from "../ds";

export function Bad() {
  const color = "#ff0000"; // rule: syntax/literal (raw hex)
  const gap = "12px"; // rule: syntax/literal (raw px)
  const font = "font-family: Helvetica"; // rule: syntax/literal (foreign font)
  const mq = "(max-width: 640px)"; // rule: syntax/literal (breakpoints come from src/ds/breakpoints.ts)
  // kivali-lint-disable-next-line copy/emoji -- wrong rule: does not cover the px on the next line
  const pad = "4px"; // rule: syntax/literal, plus kivali-lint/directive (unused) on the line above
  // kivali-lint-disable-next-line syntax/litteral -- rule: kivali-lint/directive (unknown rule)
  const bare = 1;
  // kivali-lint-disable-next-line
  const none = 2; // the line above: kivali-lint/directive (no rule list)
  return (
    <div style={{ color }}> {/* rule: react/forbid-dom-props (style) */}
      <button>raw</button> {/* rule: react/forbid-elements (button) */}
      <input /> {/* rule: react/forbid-elements (input) */}
      <select /> {/* rule: react/forbid-elements (select) */}
      <textarea /> {/* rule: react/forbid-elements (textarea) */}
      <table /> {/* rule: react/forbid-elements (table) */}
      <dialog /> {/* rule: react/forbid-elements (dialog) */}
      <h1>title</h1> {/* rule: react/forbid-elements (h1) */}
      <h2>title</h2> {/* rule: react/forbid-elements (h2) */}
      <h3>title</h3> {/* rule: react/forbid-elements (h3) */}
      <h4>title</h4> {/* rule: react/forbid-elements (h4) */}
      <h5>title</h5> {/* rule: react/forbid-elements (h5) */}
      <h6>title</h6> {/* rule: react/forbid-elements (h6) */}
      <a href="/x">link</a> {/* rule: react/forbid-elements (a) */}
      <svg /> {/* rule: react/forbid-elements (svg) */}
      <Button variant="huge">x</Button> {/* rule: syntax/enum (prop enum) */}
      <Button colour="red">x</Button> {/* rule: syntax/props (unknown prop) */}
      <Card style={{ padding: 4 }} /> {/* rule: syntax/style (style= on a component) */}
      <Badge tone="x" />
      {gap}
      {font}
      {mq}
      {pad}
      {bare}
      {none}
      <Check />
      <Dialog.Root />
      {String(styled)}
      {String(css)}
      {String(Button2)}
    </div>
  );
}
