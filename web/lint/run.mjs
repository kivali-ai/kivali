// Design-system drift linter. Usage:
//   node lint/run.mjs                 lint web/src (vendored ds files excluded)
//   node lint/run.mjs --fixtures      lint web/lint/fixtures/bad and /good (bad is expected to fail)
//   node lint/run.mjs --fixtures=bad  (or =good) lint one fixture set
// Prints one summary line per check, one `file:line [rule] message` line per problem, and a final
// pass/fail; exits 1 on any problem, and a check that crashes reports a <check>/crash problem, so a
// crash can never pass. Single-line escape hatch: see DIRECTIVE in lib.mjs.
import { spawnSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { copyLint } from "./copy-lint.mjs";
import { cssFiles, DIRECTIVE, directives, rel, tsFiles, WEB_ROOT } from "./lib.mjs";
import { syntaxCheck } from "./syntax-check.mjs";
import { tokenCheck } from "./token-check.mjs";

const args = process.argv.slice(2);
const fixtureArg = args.find((a) => a === "--fixtures" || a.startsWith("--fixtures="));

const bin = (name) => path.join(WEB_ROOT, "node_modules/.bin", name);

function runOxlint(root, hasTs) {
  if (!hasTs) return [];
  const r = spawnSync(bin("oxlint"), ["-c", "lint/oxlint.json", "--format=json", rel(root)], { cwd: WEB_ROOT, encoding: "utf8" });
  if (r.error) throw r.error;
  let report;
  try {
    report = JSON.parse(r.stdout);
  } catch {
    return [{ file: rel(root), line: 0, rule: "oxlint/crash", message: (r.stderr || r.stdout || "oxlint produced no JSON").trim().slice(0, 500) }];
  }
  // 0 = clean, 1 = diagnostics; anything else (signal, config error) is a crash even with JSON.
  if (r.status !== 0 && r.status !== 1)
    return [{ file: rel(root), line: 0, rule: "oxlint/crash", message: `oxlint exited ${r.status ?? r.signal}: ${(r.stderr || "").trim().slice(0, 500)}` }];
  if (!Array.isArray(report.diagnostics))
    return [{ file: rel(root), line: 0, rule: "oxlint/crash", message: "oxlint JSON has no diagnostics array" }];
  return report.diagnostics
    .filter((d) => d.severity === "error" || d.severity === "warning")
    .map((d) => ({
      file: d.filename,
      line: d.labels?.[0]?.span?.line ?? 0,
      rule: `oxlint/${d.code ?? "parse-error"}`,
      message: d.message + (d.help ? ` ${d.help}` : ""),
    }));
}

function runStylelint(root, files) {
  if (files.length === 0) return [];
  const r = spawnSync(bin("stylelint"), ["--config", "lint/stylelint.config.mjs", "--formatter", "json", ...files.map(rel)], {
    cwd: WEB_ROOT,
    encoding: "utf8",
  });
  if (r.error) throw r.error;
  let results;
  try {
    results = JSON.parse(r.stdout || r.stderr);
  } catch {
    return [{ file: rel(root), line: 0, rule: "stylelint/crash", message: (r.stderr || r.stdout || "stylelint produced no JSON").trim().slice(0, 500) }];
  }
  // 0 = clean, 2 = lint problems; anything else (78 config error, 1 crash, signal) fails loudly.
  if ((r.status !== 0 && r.status !== 2) || !Array.isArray(results))
    return [{ file: rel(root), line: 0, rule: "stylelint/crash", message: `stylelint exited ${r.status ?? r.signal}: ${(r.stderr || "").trim().slice(0, 500)}` }];
  const out = [];
  for (const res of results) {
    for (const w of res.warnings ?? []) out.push({ file: rel(res.source), line: w.line, rule: `stylelint/${w.rule}`, message: w.text.replace(` (${w.rule})`, "") });
    for (const e of res.parseErrors ?? []) out.push({ file: rel(res.source), line: e.line ?? 0, rule: "stylelint/parse-error", message: e.text });
    // A misconfigured rule is reported here and otherwise silently does nothing.
    for (const w of res.invalidOptionWarnings ?? []) out.push({ file: "lint/stylelint.config.mjs", line: 0, rule: "stylelint/invalid-option", message: w.text });
  }
  return out;
}

// Rules a `kivali-lint-disable-next-line` directive may name (the Node checks only; oxlint and
// stylelint have their own eslint-disable / stylelint-disable comments).
const SUPPRESSIBLE = new Set([
  "syntax/literal",
  "syntax/props",
  "syntax/enum",
  "syntax/style",
  "token-check/unknown-property",
  "token-check/app-definition",
  "token-check/raw-color",
  "token-check/font-family",
  "token-check/css-import",
  "token-check/css-location",
  "copy/banned-word",
  "copy/exclamation",
  "copy/emoji",
  "copy/title-case",
]);

// Drops problems a directive covers; returns problems for malformed, unknown and unused directives
// so every escape hatch stays deliberate and greppable.
function applyDirectives(files, results) {
  const out = [];
  for (const file of files) {
    let found;
    try {
      found = directives(file);
    } catch {
      continue; // the check that parses this file reports the parse error
    }
    for (const d of found) {
      const at = { file: rel(file), line: d.line };
      if (d.rules.length === 0) {
        out.push({ ...at, rule: "kivali-lint/directive", message: `${DIRECTIVE} needs a rule list, e.g. ${DIRECTIVE} syntax/literal -- reason.` });
        continue;
      }
      for (const rule of d.rules) {
        if (!SUPPRESSIBLE.has(rule)) {
          out.push({ ...at, rule: "kivali-lint/directive", message: `Unknown rule "${rule}" in ${DIRECTIVE}. Known: ${[...SUPPRESSIBLE].join(", ")}.` });
          continue;
        }
        let used = false;
        for (const r of results) {
          const before = r.problems.length;
          r.problems = r.problems.filter((p) => !(p.file === at.file && p.line === d.target && p.rule === rule));
          if (r.problems.length !== before) used = true;
        }
        if (!used) out.push({ ...at, rule: "kivali-lint/directive", message: `Unused ${DIRECTIVE} ${rule}: nothing on line ${d.target} to suppress.` });
      }
    }
  }
  return out;
}

function runCheck(name, fn) {
  try {
    const problems = fn();
    if (!Array.isArray(problems)) throw new Error(`${name} returned ${typeof problems}, not a problem list`);
    return problems;
  } catch (err) {
    // A parse error knows its file:line; anything else is a bug in the check, so keep the stack.
    if (err?.lintFile) return [{ file: err.lintFile, line: err.lintLine, rule: `${name}/crash`, message: err.message }];
    return [{ file: "-", line: 0, rule: `${name}/crash`, message: String(err?.stack ?? err) }];
  }
}

function lintRoot(root, label) {
  const css = cssFiles(root);
  const ts = tsFiles(root, { includeTs: true });
  const results = [
    ["oxlint", () => runOxlint(root, ts.length > 0)],
    ["syntax-check", () => syntaxCheck(root)],
    ["stylelint", () => runStylelint(root, css)],
    ["token-check", () => tokenCheck(root)],
    ["copy-lint", () => copyLint(root)],
  ].map(([name, fn]) => ({ name, problems: runCheck(name, fn) }));
  const nodeChecks = results.filter((r) => ["syntax-check", "token-check", "copy-lint"].includes(r.name));
  results.push({ name: "directives", problems: runCheck("directives", () => applyDirectives([...ts, ...css], nodeChecks)) });

  let failed = false;
  if (label) console.log(`== ${label} ==`);
  for (const { name, problems } of results) {
    problems.sort((a, b) => a.file.localeCompare(b.file) || a.line - b.line);
    console.log(`${name.padEnd(13)} ${problems.length === 0 ? "ok" : `${problems.length} problem${problems.length === 1 ? "" : "s"}`}`);
    for (const p of problems) console.log(`  ${p.file}:${p.line} [${p.rule}] ${p.message}`);
    if (problems.length > 0) failed = true;
  }
  console.log(`${label ? label + ": " : ""}${failed ? "FAIL" : "PASS"} (${ts.length} ts/tsx, ${css.length} css)`);
  return failed;
}

let failed = false;
if (fixtureArg) {
  const which = fixtureArg.includes("=") ? [fixtureArg.split("=")[1]] : ["bad", "good"];
  for (const w of which) {
    const root = path.join(WEB_ROOT, "lint/fixtures", w);
    if (!fs.existsSync(root)) {
      console.error(`no such fixture set: ${w}`);
      process.exit(2);
    }
    if (lintRoot(root, `fixtures/${w}`)) failed = true;
  }
} else {
  const root = path.join(WEB_ROOT, "src");
  if (!fs.existsSync(root)) {
    console.log("web/src does not exist yet; nothing to lint. PASS");
  } else {
    failed = lintRoot(root, "");
  }
}
process.exit(failed ? 1 : 0);
