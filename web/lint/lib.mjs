// Shared helpers for the lint stack: file discovery, TSX parsing (oxc-parser, an explicit
// devDependency and the same parser oxlint uses, so both see the same ESTree), AST walking,
// disable directives, and a uniform problem shape.
import fs from "node:fs";
import path from "node:path";
import { parseSync } from "oxc-parser";

export const WEB_ROOT = path.resolve(path.dirname(new URL(import.meta.url).pathname), "..");

// Vendored copies of the designer's files. Never linted.
const VENDORED = [/[\\/]ds[\\/]kivali\.css$/, /[\\/]ds[\\/]tokens[\\/]/, /[\\/]ds[\\/]fonts[\\/]/];

export function isVendored(file) {
  return VENDORED.some((re) => re.test(file));
}

// True for files that live under a src/ds directory (the ported design-system components).
export function isDs(file) {
  return /[\\/]src[\\/]ds[\\/]/.test(file);
}

export function isTestOrGenerated(file) {
  return /\.(test|spec)\.[cm]?[jt]sx?$/.test(file) || /\.gen\.tsx?$/.test(file);
}

export function walkFiles(root, exts) {
  const out = [];
  if (!fs.existsSync(root)) return out;
  const visit = (dir) => {
    for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
      if (entry.name === "node_modules" || entry.name.startsWith(".")) continue;
      const full = path.join(dir, entry.name);
      if (entry.isDirectory()) visit(full);
      else if (exts.some((e) => entry.name.endsWith(e))) out.push(full);
    }
  };
  visit(root);
  return out.sort();
}

export function cssFiles(root) {
  return walkFiles(root, [".css"]).filter((f) => !isVendored(f));
}

// TSX/TS app sources: no tests, no generated files.
export function tsFiles(root, { includeTs = false } = {}) {
  const exts = includeTs ? [".tsx", ".ts"] : [".tsx"];
  return walkFiles(root, exts).filter((f) => !isTestOrGenerated(f) && !f.endsWith(".d.ts"));
}

export function rel(file) {
  return path.relative(WEB_ROOT, file);
}

export function lineOf(text, offset) {
  let line = 1;
  for (let i = 0; i < offset && i < text.length; i++) if (text.charCodeAt(i) === 10) line++;
  return line;
}

export function problem(file, line, rule, message) {
  return { file: rel(file), line, rule, message };
}

// The directory a lint root's paths are relative to: web/ for web/src, the fixture set itself for
// lint/fixtures/<set> (which mirrors the src/ layout underneath).
export function projectBase(root) {
  return path.basename(root) === "src" ? path.dirname(root) : root;
}

// Parses once per file per process; every check shares the result. A parse error throws, so the
// calling check crashes and run.mjs reports it as a failure rather than silently skipping the file.
const parsed = new Map();
export function parseTsx(file) {
  if (parsed.has(file)) return parsed.get(file);
  const text = fs.readFileSync(file, "utf8");
  const lang = file.endsWith(".tsx") ? "tsx" : "ts";
  const r = parseSync(file, text, { lang, sourceType: "module" });
  if (r.errors.length > 0) {
    const e = r.errors[0];
    const at = e.labels?.[0]?.start;
    const err = new Error(`parse error: ${e.message}`);
    err.lintFile = rel(file);
    err.lintLine = at === undefined ? 0 : lineOf(text, at);
    throw err;
  }
  const out = { text, ast: r.program, comments: r.comments };
  parsed.set(file, out);
  return out;
}

// Escape hatch, same shape as oxlint's eslint-disable-next-line:
//   // kivali-lint-disable-next-line syntax/literal -- why
//   {/* kivali-lint-disable-next-line copy/exclamation, copy/title-case -- why */}
//   /* kivali-lint-disable-next-line token-check/raw-color -- why */   (CSS)
// A rule list is required (no blanket disables) and applies to the line after the comment ends.
// Only the Node checks (syntax/, token-check/, copy/) honour it; oxlint and stylelint use their own.
export const DIRECTIVE = "kivali-lint-disable-next-line";
const DIRECTIVE_RE = /^\s*kivali-lint-disable-next-line\b([^\n]*)$/;

// Returns [{ line, target, rules: [..] }] for every directive in the file (line = the comment's line,
// target = the line it suppresses). Malformed directives come back with rules: [].
export function directives(file) {
  const isCss = file.endsWith(".css");
  let comments;
  let text;
  if (isCss) {
    text = fs.readFileSync(file, "utf8");
    comments = [...text.matchAll(/\/\*([\s\S]*?)\*\//g)].map((m) => ({ value: m[1], start: m.index, end: m.index + m[0].length }));
  } else {
    ({ text, comments } = parseTsx(file));
  }
  const out = [];
  for (const c of comments) {
    const m = c.value.trim().match(DIRECTIVE_RE);
    if (!m) continue;
    const list = m[1].split(/\s--\s|\s--$/)[0];
    const rules = list
      .split(/[\s,]+/)
      .map((r) => r.trim())
      .filter(Boolean);
    out.push({ line: lineOf(text, c.start), target: lineOf(text, c.end) + 1, rules });
  }
  return out;
}

// Depth-first walk over an ESTree node. visit(node, parent) may return false to skip children.
export function walk(node, visit, parent = null) {
  if (!node || typeof node.type !== "string") return;
  if (visit(node, parent) === false) return;
  for (const key of Object.keys(node)) {
    const v = node[key];
    if (Array.isArray(v)) for (const c of v) walk(c, visit, node);
    else if (v && typeof v === "object") walk(v, visit, node);
  }
}

// Every string a UI could show or a CSS value could hide: string literals, template quasis, JSX text.
export function strings(ast) {
  const out = [];
  walk(ast, (n, parent) => {
    if (n.type === "Literal" && typeof n.value === "string") {
      const isImport =
        parent && /^(ImportDeclaration|ExportAllDeclaration|ExportNamedDeclaration)$/.test(parent.type);
      out.push({ value: n.value, start: n.start, parent, isImport, kind: "literal" });
    } else if (n.type === "TemplateElement") {
      out.push({ value: n.value.cooked ?? n.value.raw, start: n.start, parent, kind: "template" });
    } else if (n.type === "JSXText") {
      out.push({ value: n.value, start: n.start, parent, kind: "jsxtext" });
    }
  });
  return out;
}

// Strip CSS comments but keep newlines so line numbers survive.
export function stripCssComments(text) {
  return text.replace(/\/\*[\s\S]*?\*\//g, (m) => m.replace(/[^\n]/g, " "));
}
