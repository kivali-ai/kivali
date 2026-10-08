// Brand copy rules (design-system/README.md CONTENT FUNDAMENTALS) over JSX text and string literals.
//   copy/banned-word   simply, seamless, bots, magic, AI-powered (case-insensitive, whole words)
//   copy/exclamation   exclamation marks in UI strings
//   copy/emoji         any Extended_Pictographic character
//   copy/title-case    Title Case labels: <Button> children and label=/title= props (sentence case only)
// Skips tests, *.gen.ts and src/ds (the designer's reference copy).
import fs from "node:fs";
import path from "node:path";
import { isDs, lineOf, parseTsx, problem, strings, tsFiles, walk, WEB_ROOT } from "./lib.mjs";

const BANNED = /\b(?:simply|seamless|bots|magic|AI-powered)\b/i;
const EXCLAMATION = /(?<![!=\s])!(?![=\w])/;
const EMOJI = /\p{Extended_Pictographic}/u;
// Attributes whose values are identifiers, paths or class names rather than prose.
const NON_PROSE_ATTRS = new Set(["className", "key", "id", "to", "href", "htmlFor", "type", "role", "name", "src", "rel", "target", "d", "viewBox"]);

function loadAllowlist() {
  const file = path.join(WEB_ROOT, "lint/copy-allowlist.txt");
  if (!fs.existsSync(file)) return [];
  return fs
    .readFileSync(file, "utf8")
    .split("\n")
    .map((l) => l.trim())
    .filter((l) => l && !l.startsWith("#"))
    .sort((a, b) => b.length - a.length); // longest phrase first
}

// Two or more words where a later word starts uppercase (and is not an acronym or allowlisted).
export function titleCaseProblem(text, allowlist) {
  let t = text.replace(/\s+/g, " ").trim();
  for (const a of allowlist) t = t.split(a).join(" ");
  t = t.replace(/#\d+/g, " ");
  const words = t.split(" ").filter(Boolean);
  if (words.length < 2) return null;
  for (let i = 1; i < words.length; i++) {
    const prev = words[i - 1];
    if (/[.?:·]$/.test(prev)) continue; // a new sentence may start uppercase
    if (/^["'(]?[A-Z][a-z]/.test(words[i])) return words[i].replace(/[^\w]/g, "");
  }
  return null;
}

function staticStrings(node) {
  const out = [];
  walk(node, (n) => {
    if (n.type === "Literal" && typeof n.value === "string") out.push({ value: n.value, start: n.start });
    else if (n.type === "JSXText") out.push({ value: n.value, start: n.start });
    else if (n.type === "TemplateElement") out.push({ value: n.value.cooked ?? n.value.raw, start: n.start });
  });
  return out;
}

export function copyLint(root) {
  const problems = [];
  const allowlist = loadAllowlist();
  for (const file of tsFiles(root, { includeTs: true }).filter((f) => !isDs(f))) {
    const { text, ast } = parseTsx(file);
    const seen = new Set();
    const add = (start, rule, message) => {
      const key = `${start}:${rule}`;
      if (seen.has(key)) return;
      seen.add(key);
      problems.push(problem(file, lineOf(text, start), rule, message));
    };

    for (const s of strings(ast)) {
      if (s.isImport) continue;
      if (s.parent && s.parent.type === "JSXAttribute" && NON_PROSE_ATTRS.has(s.parent.name?.name)) continue;
      const v = s.value;
      const banned = v.match(BANNED);
      if (banned) add(s.start, "copy/banned-word", `Banned word "${banned[0]}" (simply, seamless, bots, magic, AI-powered).`);
      if (EXCLAMATION.test(v)) add(s.start, "copy/exclamation", `Exclamation mark in UI string "${v.trim().slice(0, 40)}". The voice is calm.`);
      if (EMOJI.test(v)) add(s.start, "copy/emoji", `Emoji or pictographic glyph in "${v.trim().slice(0, 40)}". Use the Icon component.`);
    }

    // Title Case: <Button> children, and label= / title= props on any element.
    walk(ast, (n) => {
      if (n.type === "JSXElement" && n.openingElement.name.type === "JSXIdentifier" && n.openingElement.name.name === "Button") {
        for (const child of n.children) {
          if (child.type === "JSXElement" || child.type === "JSXFragment") continue;
          for (const s of staticStrings(child)) {
            const bad = titleCaseProblem(s.value, allowlist);
            if (bad) add(s.start, "copy/title-case", `Title Case in a button label ("${s.value.trim()}", "${bad}"). Use sentence case, or add a proper noun to lint/copy-allowlist.txt.`);
          }
        }
      }
      if (n.type === "JSXAttribute" && n.name.type === "JSXIdentifier" && (n.name.name === "label" || n.name.name === "title") && n.value) {
        for (const s of staticStrings(n.value)) {
          const bad = titleCaseProblem(s.value, allowlist);
          if (bad) add(s.start, "copy/title-case", `Title Case in ${n.name.name}= ("${s.value.trim()}", "${bad}"). Use sentence case, or add a proper noun to lint/copy-allowlist.txt.`);
        }
      }
    });
  }
  return problems;
}
