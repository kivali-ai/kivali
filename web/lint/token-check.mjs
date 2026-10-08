// Design-token drift check over app CSS and TSX.
//   token-check/unknown-property  var(--x) where --x is not a design-system property
//   token-check/app-definition    an app definition of a custom property not prefixed --app-
//   token-check/raw-color         hex, rgb(), hsl() (and friends) anywhere in app CSS or TSX strings
//   token-check/font-family       a font-family that is not Space Grotesk, IBM Plex Sans/Mono, or a token
//   token-check/css-import        @import of anything outside src/ds
//   token-check/css-location      an app CSS file outside src/styles/
import fs from "node:fs";
import path from "node:path";
import { cssFiles, isDs, lineOf, parseTsx, problem, projectBase, stripCssComments, strings, tsFiles, walk, walkFiles, WEB_ROOT } from "./lib.mjs";

const REPO_ROOT = path.resolve(WEB_ROOT, "..");

// Custom properties the design system defines or uses. Sources: tokens.json (every group), the
// tokens/*.css and kivali.css in design-system/ (the source of truth) and in web/src/ds (the synced copies),
// plus --kv-* properties DEFINED (not merely used) by Kivali's own CSS under the linted tree's src/ds.
export function legalProperties(root = path.join(WEB_ROOT, "src")) {
  const legal = new Set();
  const kv = new Set();
  const tokens = JSON.parse(fs.readFileSync(path.join(REPO_ROOT, "design-system/tokens/tokens.json"), "utf8"));
  for (const group of Object.values(tokens)) {
    if (group && Array.isArray(group.tokens)) for (const t of group.tokens) legal.add(`--${t.name}`);
  }
  const sources = [
    ...walkFiles(path.join(REPO_ROOT, "design-system/tokens"), [".css"]),
    path.join(REPO_ROOT, "design-system/components/kivali.css"),
    ...walkFiles(path.join(WEB_ROOT, "src/ds/tokens"), [".css"]),
    path.join(WEB_ROOT, "src/ds/kivali.css"),
  ];
  for (const f of sources) {
    if (!fs.existsSync(f)) continue;
    for (const m of stripCssComments(fs.readFileSync(f, "utf8")).matchAll(/(--[a-zA-Z][\w-]*)/g)) {
      legal.add(m[1]);
      if (m[1].startsWith("--kv-")) kv.add(m[1]);
    }
  }
  for (const f of cssFiles(path.join(projectBase(root), "src/ds"))) {
    for (const m of stripCssComments(fs.readFileSync(f, "utf8")).matchAll(/(--kv-[\w-]+)\s*:/g)) {
      legal.add(m[1]);
      kv.add(m[1]);
    }
  }
  return { legal, kv };
}

const COLOR_FN = /\b(?:rgba?|hsla?|hwb|oklch|oklab|lab|lch)\(/i;
const HEX = /(?<![\w&/])#(?:[0-9a-fA-F]{8}|[0-9a-fA-F]{6}|[0-9a-fA-F]{3,4})\b(?<!#\d+)/;
const BRAND_FONTS = new Set(["space grotesk", "ibm plex sans", "ibm plex mono"]);
const GENERIC_FONTS = new Set(["sans-serif", "serif", "monospace", "system-ui", "inherit", "initial", "unset"]);

// A font-family value is fine when it is a var() token or lists only brand fonts and generic keywords.
export function fontFamilyOk(value) {
  const v = value.trim().replace(/\s*!important$/i, "");
  if (/^var\(/.test(v)) return true;
  return v.split(",").every((part) => {
    const name = part.trim().replace(/^["']|["']$/g, "").toLowerCase();
    return BRAND_FONTS.has(name) || GENERIC_FONTS.has(name) || /^var\(/.test(name);
  });
}

// A var() reference is legal when the property is a token, an app property, a Radix-provided one,
// or a dynamic prefix such as `var(--identity-${name})` that some token extends.
function isLegalUse(name, legal) {
  if (legal.has(name) || name.startsWith("--app-") || name.startsWith("--radix-")) return true;
  if (name.endsWith("-")) for (const l of legal) if (l.startsWith(name)) return true;
  return false;
}

const VAR_USE =/var\(\s*(--[\w-]+)/g;
const PROP_DEF = /(?:^|[\s;{"'])(--[\w-]+)\s*:/g;

function checkProperties(text, file, lineAt, allowKvDef, ctx, problems) {
  const { legal, kv } = ctx;
  for (const m of text.matchAll(VAR_USE)) {
    const name = m[1];
    if (!isLegalUse(name, legal))
      problems.push(problem(file, lineAt(m.index), "token-check/unknown-property", `var(${name}) is not a design-system token or --kv-* property.`));
  }
  for (const m of text.matchAll(PROP_DEF)) {
    const name = m[1];
    if (name.startsWith("--app-")) continue;
    if (allowKvDef === "any" && name.startsWith("--kv-")) continue; // src/ds CSS defines the design system's own properties
    if (allowKvDef && kv.has(name)) continue; // TSX may pass a documented --kv-* variable, e.g. --kv-autorel-frac
    problems.push(problem(file, lineAt(m.index), "token-check/app-definition", `${name} is defined in app code; app custom properties must be prefixed --app-.`));
  }
}

// A relative @import is resolved against the importing file and must land in src/ds (so
// src/ds/index.css may import ./tokens/*.css). URLs and bare specifiers are never design-system files.
function importTargetIsDs(file, target) {
  if (!/^\.{1,2}\//.test(target)) return false;
  return isDs(path.resolve(path.dirname(file), target));
}

export function tokenCheck(root) {
  const problems = [];
  const ctx = legalProperties(root);

  for (const file of cssFiles(root)) {
    const raw = fs.readFileSync(file, "utf8");
    const text = stripCssComments(raw);
    const lineAt = (i) => lineOf(text, i);
    checkProperties(text, file, lineAt, isDs(file) ? "any" : false, ctx, problems);
    for (const m of text.matchAll(/([\w-]+)\s*:\s*([^;{}]+)/g)) {
      const [, prop, value] = m;
      if (HEX.test(value) || COLOR_FN.test(value))
        problems.push(problem(file, lineAt(m.index), "token-check/raw-color", `Raw color in ${prop}: ${value.trim()}. Use a color token via var().`));
      if (prop === "font-family" && !fontFamilyOk(value))
        problems.push(problem(file, lineAt(m.index), "token-check/font-family", `font-family "${value.trim()}" is not Space Grotesk, IBM Plex Sans, IBM Plex Mono or a token.`));
    }
    for (const m of text.matchAll(/@import\s+(?:url\(\s*)?["']?([^"')\s;]+)/g)) {
      const target = m[1];
      if (!importTargetIsDs(file, target))
        problems.push(problem(file, lineOf(text, m.index), "token-check/css-import", `@import "${target}" is outside src/ds. CSS imports nothing but design-system files.`));
    }
    // App classes live in src/styles/ (the design system's own CSS lives in src/ds/).
    const where = path.relative(projectBase(root), file).split(path.sep).join("/");
    if (!isDs(file) && !where.startsWith("src/styles/"))
      problems.push(problem(file, 1, "token-check/css-location", `${where} is outside src/styles/. App CSS lives in src/styles/.`));
  }

  for (const file of tsFiles(root)) {
    const ds = isDs(file);
    const { text, ast } = parseTsx(file);
    for (const s of strings(ast)) {
      if (s.isImport) continue;
      const lineAt = (i) => lineOf(text, s.start) + (s.value.slice(0, i).match(/\n/g) || []).length;
      if (ds) {
        // The ported components: only catch typos in var(--x) references.
        for (const m of s.value.matchAll(VAR_USE)) {
          const name = m[1];
          if (!isLegalUse(name, ctx.legal))
            problems.push(problem(file, lineAt(m.index), "token-check/unknown-property", `var(${name}) is not a design-system token or --kv-* property.`));
        }
        continue;
      }
      checkProperties(s.value, file, lineAt, true, ctx, problems);
      if (s.kind !== "jsxtext" && (HEX.test(s.value) || COLOR_FN.test(s.value)))
        problems.push(problem(file, lineOf(text, s.start), "token-check/raw-color", `Raw color in string "${s.value.trim().slice(0, 40)}". Use a color token via var().`));
      for (const m of s.value.matchAll(/font-family\s*:\s*([^;]+)/gi))
        if (!fontFamilyOk(m[1]))
          problems.push(problem(file, lineOf(text, s.start), "token-check/font-family", `font-family "${m[1].trim()}" is not Space Grotesk, IBM Plex Sans, IBM Plex Mono or a token.`));
    }
    if (ds) continue;
    // Object keys: { '--kv-autorel-frac': 0.4 } is a definition; { fontFamily: 'Georgia' } is a font.
    walk(ast, (n) => {
      if (n.type !== "Property" || n.computed) return;
      const key = n.key.type === "Identifier" ? n.key.name : n.key.type === "Literal" ? String(n.key.value) : "";
      if (key.startsWith("--") && !key.startsWith("--app-") && !ctx.kv.has(key))
        problems.push(problem(file, lineOf(text, n.start), "token-check/app-definition", `${key} is defined in app code; app custom properties must be prefixed --app-.`));
      if (key === "fontFamily" && n.value.type === "Literal" && typeof n.value.value === "string" && !fontFamilyOk(n.value.value))
        problems.push(problem(file, lineOf(text, n.start), "token-check/font-family", `fontFamily "${n.value.value}" is not Space Grotesk, IBM Plex Sans, IBM Plex Mono or a token.`));
    });
  }
  return problems;
}
