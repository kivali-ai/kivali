// Design-system pieces as DOM: the `kv-*` classes from
// design-system/components/kivali.css plus the desktop's own (style.css).
// Sizes that depend on a prop are set through CSSOM (el.style), which the
// CSP allows; markup carries no style attributes.

import { cx, h, type Child } from "./h";
import { icon, type IconName } from "./icons";
import { initials } from "./logic/format";
import { dotOf } from "./logic/teams";
import type { TeamState } from "./types";

export type Variant = "primary" | "secondary" | "ghost" | "danger";

export interface ButtonOpts {
  variant?: Variant;
  size?: "sm" | "md";
  onclick?: (e: Event) => void;
  disabled?: boolean;
  loading?: boolean;
  id?: string;
  type?: "button" | "submit";
  cls?: string;
  icon?: IconName;
  label?: string;
}

export function button(text: string, o: ButtonOpts = {}): HTMLButtonElement {
  const b = h(
    "button",
    {
      type: o.type ?? "button",
      class: cx("kv-btn", `kv-btn--${o.variant ?? "secondary"}`, `kv-btn--${o.size ?? "md"}`, o.loading && "is-loading", o.cls),
      id: o.id,
      disabled: o.disabled || o.loading,
      "aria-busy": o.loading ? "true" : null,
      "aria-label": o.label,
      onclick: o.onclick,
    },
    o.loading ? h("span", { class: "kv-btn-dots", "aria-hidden": "true" }, h("i"), h("i"), h("i")) : o.icon ? icon(o.icon, 16) : null,
    h("span", { class: "kv-btn-label" }, text),
  );
  return b;
}

export interface FieldOpts {
  id: string;
  label: string;
  value?: string;
  hint?: string | Node | null;
  error?: string | null;
  type?: string;
  placeholder?: string;
  autocomplete?: string;
  inputmode?: string;
  oninput?: (v: string) => void;
  onenter?: () => void;
  onblur?: (input: HTMLInputElement) => void;
  disabled?: boolean;
}

export function field(o: FieldOpts): { wrap: HTMLDivElement; input: HTMLInputElement } {
  const msg = o.error || o.hint ? `${o.id}-msg` : null;
  const input = h("input", {
    id: o.id,
    class: "kv-input",
    type: o.type ?? "text",
    placeholder: o.placeholder,
    autocomplete: o.autocomplete ?? "off",
    inputmode: o.inputmode,
    spellcheck: "false",
    autocapitalize: "off",
    disabled: o.disabled,
    "aria-invalid": o.error ? "true" : null,
    "aria-describedby": msg,
  });
  input.value = o.value ?? "";
  input.addEventListener("input", () => o.oninput?.(input.value));
  if (o.onenter)
    input.addEventListener("keydown", (e) => {
      if (e.key === "Enter" && !e.isComposing) {
        e.preventDefault();
        o.onenter!();
      }
    });
  if (o.onblur) input.addEventListener("blur", () => o.onblur!(input));
  const wrap = h(
    "div",
    { class: cx("kv-field", o.error && "is-invalid") },
    h("label", { class: "kv-field-label", for: o.id }, o.label),
    input,
    o.error
      ? h("p", { class: "kv-field-error", id: msg, role: "alert" }, icon("triangle-alert", 14), o.error)
      : o.hint
        ? h("p", { class: "kv-field-hint", id: msg }, o.hint)
        : null,
  );
  return { wrap, input };
}

export function select(o: {
  id: string;
  label: string;
  options: { value: number | string; label: string }[];
  value: number | string;
  onchange: (v: string) => void;
  disabled?: boolean;
}): HTMLDivElement {
  const sel = h("select", { id: o.id, class: "kv-input", disabled: o.disabled });
  for (const opt of o.options) {
    const el = h("option", { value: String(opt.value) }, opt.label);
    if (String(opt.value) === String(o.value)) el.selected = true;
    sel.appendChild(el);
  }
  sel.addEventListener("change", () => o.onchange(sel.value));
  return h(
    "div",
    { class: "kv-field" },
    h("label", { class: "kv-field-label", for: o.id }, o.label),
    h("span", { class: "kv-select" }, sel, icon("chevron-down", 16)),
  );
}

export function switchRow(o: { id: string; label: string; hint?: string; checked: boolean; onchange: (v: boolean) => void }) {
  const state = o.checked ? "checked" : "unchecked";
  const btn = h(
    "button",
    {
      type: "button",
      role: "switch",
      id: o.id,
      class: "kv-switch",
      "data-state": state,
      "aria-checked": String(o.checked),
      "aria-describedby": o.hint ? `${o.id}-hint` : null,
      onclick: () => o.onchange(!o.checked),
    },
    h("span", { class: "kv-switch-thumb", "data-state": state }),
  );
  return h(
    "div",
    { class: "kv-choice kv-choice--switch" },
    btn,
    h("label", { for: o.id }, h("span", {}, o.label), o.hint ? h("small", { id: `${o.id}-hint` }, o.hint) : null),
  );
}

const BANNER_ICON: Record<string, IconName> = { info: "info", warning: "triangle-alert", danger: "triangle-alert", success: "circle-check" };

export function banner(tone: "info" | "warning" | "danger" | "success", title: string, body?: string | null): HTMLDivElement {
  return h(
    "div",
    { class: cx("kv-banner", `kv-banner--${tone}`), role: tone === "danger" || tone === "warning" ? "alert" : "status" },
    icon(BANNER_ICON[tone], 18),
    h("div", { class: "kv-banner-text" }, h("strong", {}, title), body ? h("span", {}, body) : null),
  );
}

/** A team's mark: initials on slate, rounded at 24% of its size. */
export function orgMark(name: string, size: number, elsewhere = false): HTMLElement {
  const ini = name
    .trim()
    .split(/\s+/)
    .map((w) => w[0] ?? "")
    .join("")
    .slice(0, 2)
    .toUpperCase();
  const mark = h("span", { class: "kv-orgmark orgmark", role: "img", "aria-label": name }, ini);
  mark.style.width = `${size}px`;
  mark.style.height = `${size}px`;
  mark.style.borderRadius = `${Math.round(size * 0.24)}px`;
  mark.style.fontSize = `${Math.round(size * 0.42)}px`;
  if (!elsewhere) return mark;
  const badge = h("span", { class: "orgmark-globe", "aria-hidden": "true" }, icon("globe", Math.max(10, Math.round(size * 0.26))));
  const s = Math.max(14, Math.round(size * 0.4));
  badge.style.width = `${s}px`;
  badge.style.height = `${s}px`;
  return h("span", { class: "orgmark-wrap" }, mark, badge);
}

export function personAvatar(name: string, size: number): HTMLElement {
  const a = h("span", { class: "kv-avatar kv-avatar--person", role: "img", "aria-label": name }, h("span", { class: "kv-avatar-initials" }, initials(name)));
  a.style.width = `${size}px`;
  a.style.height = `${size}px`;
  a.style.fontSize = `${Math.round(size * 0.38)}px`;
  return a;
}

/** The design system's Progress, with its label row ("Progress 45%"). */
export function progress(percent: number, tone: "ink" | "cobalt", label = "Progress"): HTMLElement {
  const pct = Math.max(0, Math.min(100, Math.round(percent)));
  const fill = h("span");
  fill.style.width = `${pct}%`;
  return h(
    "div",
    { class: "kv-progress-wrap" },
    h("div", { class: "kv-progress-label" }, h("span", {}, label), h("span", {}, `${pct}%`)),
    h(
      "div",
      {
        class: cx("kv-progress", `kv-progress--${tone}`),
        role: "progressbar",
        "aria-valuenow": pct,
        "aria-valuemin": 0,
        "aria-valuemax": 100,
        "aria-label": label,
      },
      fill,
    ),
  );
}

/** A team's dot. Starting pulses, in step across re-renders. */
export function dot(state: TeamState, size: 8 | 10 | 11 = 8): HTMLElement {
  const d = h("span", { class: cx("dot", `dot--${size}`, dotOf(state).cls), "aria-hidden": "true" });
  if (state === "starting") d.style.animationDelay = `-${Date.now() % 1600}ms`;
  return d;
}

export function readyDot(): HTMLElement {
  return h("span", { class: "dot dot--8 dot--running", "aria-hidden": "true" });
}

export function monoLabel(text: string): HTMLElement {
  return h("span", { class: "mono-label" }, text);
}

/** A raised container of hairline-divided rows. */
export function card(...rows: Child[]): HTMLElement {
  return h("div", { class: "box" }, ...rows);
}

/** A Settings row: title + line on the left, controls on the right. */
export function row(o: { title: Child; sub?: Child | Child[]; lead?: Child; trail?: Child | Child[]; cls?: string }): HTMLElement {
  return h(
    "div",
    { class: cx("srow", o.cls) },
    o.lead ?? null,
    h(
      "div",
      { class: "srow-main" },
      h("span", { class: "srow-title" }, o.title),
      ...(Array.isArray(o.sub) ? o.sub : [o.sub]).map((s) => (s ? (typeof s === "string" ? h("span", { class: "caption muted" }, s) : s) : null)),
    ),
    ...(Array.isArray(o.trail) ? o.trail : [o.trail]),
  );
}

/** A 40px round disc holding an icon or an avatar. */
export function disc(content: Node, tone: "plain" | "success" = "plain"): HTMLElement {
  return h("span", { class: cx("disc", tone === "success" && "disc--success") }, content);
}

/** The round radio mark on choice cards and rows. */
export function radioMark(on: boolean): HTMLElement {
  return h("span", { class: cx("radio-mark", on && "is-on"), "aria-hidden": "true" }, on ? icon("check", 16) : null);
}

// ---- Sheets: in-page dialogs attached to the top of the window ----

export interface SheetOpts {
  key: string;
  title: string;
  body: Child[];
  buttons: HTMLButtonElement[];
  /** The button Return activates when focus isn't in a field. */
  defaultButton?: HTMLButtonElement;
  /** The element to focus when it opens. */
  focus?: HTMLElement;
  onCancel: () => void;
}

let lastSheetKey: string | null = null;

/** The overlay + sheet. Re-rendering the same sheet skips the entrance animation. */
export function sheet(o: SheetOpts): HTMLElement {
  const fresh = lastSheetKey !== o.key;
  lastSheetKey = o.key;
  const titleId = `sheet-title-${o.key}`;
  const box = h(
    "div",
    { class: "sheet", role: "dialog", "aria-modal": "true", "aria-labelledby": titleId, tabindex: "-1" },
    h("h2", { class: "sheet-title", id: titleId }, o.title),
    ...o.body,
    h("div", { class: "sheet-foot" }, ...o.buttons),
  );
  const overlay = h("div", { class: cx("sheet-overlay", !fresh && "is-static") }, box);
  overlay.addEventListener("mousedown", (e) => {
    if (e.target === overlay) o.onCancel();
  });
  box.addEventListener("keydown", (e) => {
    if (e.key === "Escape") {
      e.preventDefault();
      o.onCancel();
    } else if (e.key === "Tab") {
      const els = [...box.querySelectorAll<HTMLElement>("button:not([disabled]), input:not([disabled]), select, [href]")];
      if (!els.length) return;
      const first = els[0];
      const last = els[els.length - 1];
      if (e.shiftKey && document.activeElement === first) {
        e.preventDefault();
        last.focus();
      } else if (!e.shiftKey && document.activeElement === last) {
        e.preventDefault();
        first.focus();
      }
    } else if (e.key === "Enter" && o.defaultButton && !(e.target instanceof HTMLInputElement) && !(e.target instanceof HTMLButtonElement)) {
      e.preventDefault();
      if (!o.defaultButton.disabled) o.defaultButton.click();
    }
  });
  if (fresh) queueMicrotask(() => (o.focus ?? o.defaultButton ?? box).focus());
  return overlay;
}

export function closeSheet() {
  lastSheetKey = null;
}

export function caption(text: Child, cls = ""): HTMLElement {
  return h("span", { class: cx("caption muted", cls) }, text);
}

export function settingsButtonLabel(platform: string): string {
  return platform === "macos" ? "Show in Finder" : platform === "windows" ? "Show in Explorer" : "Show in folder";
}
