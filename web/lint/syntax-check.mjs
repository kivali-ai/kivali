// The designer's no-restricted-syntax rules (raw hex, raw px, foreign font-family, prop enums,
// unknown props). oxlint 1.86 does not implement no-restricted-syntax, so the same selectors run
// here over an oxc AST. Rules live in syntax-rules.json (from design-system/_adherence.oxlintrc.json).
//   syntax/literal  hex, px, font-family in any string literal or template chunk (.ts and .tsx)
//   syntax/props    a prop a design-system component does not declare
//   syntax/enum     a literal prop value outside the component's enum
//   syntax/style    style= on a component that passes anything but a documented --kv-* variable
// The designer's config applies to the components too, so src/ds is linted like app code, with
// two differences: syntax/style does not apply inside src/ds, and a literal rule may name exact
// files it exempts (syntax-rules.json "exemptFiles", paths relative to web/). There are no globs.
import fs from "node:fs";
import path from "node:path";
import { isDs, lineOf, parseTsx, problem, projectBase, tsFiles, walk, WEB_ROOT } from "./lib.mjs";

const spec = JSON.parse(fs.readFileSync(path.join(WEB_ROOT, "lint/syntax-rules.json"), "utf8"));

const literalRules = spec.rules
  .filter((r) => r.kind === "literal")
  .map((r) => ({ ...r, re: new RegExp(r.pattern, r.flags), exempt: new Set(r.exemptFiles ?? []) }));
const propRules = new Map(
  spec.rules.filter((r) => r.kind === "props").map((r) => [r.component, { ...r, re: new RegExp(`^(?:${r.allowed})$`) }]),
);
const enumRules = spec.rules
  .filter((r) => r.kind === "enum")
  .map((r) => ({ ...r, re: new RegExp(`^(?:${r.allowed})$`) }));

// A JSX name that is a component (Button, Dialog.Root), not a DOM element (div). oxlint's
// react/forbid-dom-props already covers style= on DOM elements.
function isComponentName(name) {
  if (name.type === "JSXMemberExpression") return true;
  return name.type === "JSXIdentifier" && /^[A-Z]/.test(name.name);
}

// style={{ "--kv-autorel-frac": x }} is the only allowed shape: an object literal whose keys are all
// --kv-* strings (token-check separately verifies the variable exists).
function styleIsKvVarsOnly(attr) {
  const v = attr.value;
  if (!v || v.type !== "JSXExpressionContainer" || v.expression.type !== "ObjectExpression") return false;
  return v.expression.properties.every(
    (p) => p.type === "Property" && !p.computed && p.key.type === "Literal" && typeof p.key.value === "string" && p.key.value.startsWith("--kv-"),
  );
}

export function syntaxCheck(root) {
  const problems = [];
  const base = projectBase(root);
  for (const file of tsFiles(root, { includeTs: true })) {
    const relPath = path.relative(base, file).split(path.sep).join("/");
    const ds = isDs(file);
    const rules = literalRules.filter((r) => !r.exempt.has(relPath));
    const { text, ast } = parseTsx(file);
    const checkLiteral = (value, start) => {
      for (const r of rules) if (r.re.test(value)) problems.push(problem(file, lineOf(text, start), "syntax/literal", r.message));
    };
    walk(ast, (n, parent) => {
      const isImport = parent && /^(Import|Export)/.test(parent.type);
      if (!isImport && n.type === "Literal" && typeof n.value === "string") {
        checkLiteral(n.value, n.start);
      } else if (n.type === "TemplateElement") {
        checkLiteral(n.value.cooked ?? n.value.raw, n.start);
      } else if (n.type === "JSXOpeningElement") {
        const comp = n.name.type === "JSXIdentifier" ? n.name.name : null;
        const pr = comp && propRules.get(comp);
        for (const attr of n.attributes) {
          if (attr.type !== "JSXAttribute" || attr.name.type !== "JSXIdentifier") continue;
          const prop = attr.name.name;
          if (pr && !pr.re.test(prop))
            problems.push(problem(file, lineOf(text, attr.start), "syntax/props", `${pr.message} Got "${prop}".`));
          for (const er of enumRules) {
            if (er.component !== comp || er.prop !== prop) continue;
            const v = attr.value;
            if (v && v.type === "Literal" && typeof v.value === "string" && !er.re.test(v.value))
              problems.push(problem(file, lineOf(text, attr.start), "syntax/enum", `${er.message} Got "${v.value}".`));
          }
          if (!ds && prop === "style" && isComponentName(n.name) && !styleIsKvVarsOnly(attr))
            problems.push(
              problem(
                file,
                lineOf(text, attr.start),
                "syntax/style",
                "No style= outside src/ds except an object of documented --kv-* variables (style={{ \"--kv-autorel-frac\": f }}). Use an app- class with tokens.",
              ),
            );
        }
      }
    });
  }
  return problems;
}
