// The Microsoft Foundry sheet, opened from setup step 3 and Settings → AI:
// the form, then what Claude and each model's deployment said. It sends a
// sign-in setup (apply_credential_setup); the words and checks are
// logic/foundry.ts.

import type { Ctx } from "../ctx";
import { h, type Child } from "../h";
import { foundryErrors, foundryOutcome, foundrySetup, missingTitle, newFoundryForm, type FoundryForm } from "../logic/foundry";
import { errorText } from "../logic/setup";
import type { SetupInfo, SetupResult, Team } from "../types";
import { banner, button, caption, field, select, sheet } from "../ui";

interface State {
  team: string;
  info: SetupInfo | null;
  form: FoundryForm;
  /** Show field errors: set by the first Save. */
  tried: boolean;
  busy: boolean;
  error: string | null;
  result: SetupResult | null;
}

let st: State | null = null;

const p = (text: Child) => h("p", { class: "sheet-text" }, text);

/** Opens the sheet for a team, starting from the setup saved now. */
export function openFoundry(ctx: Ctx, t: Team) {
  const mine: State = { team: t.id, info: null, form: newFoundryForm(), tried: false, busy: false, error: null, result: null };
  st = mine;
  ctx.rerender();
  ctx
    .act<SetupInfo>("credential_setup", { id: t.id }, true)
    .then((info) => {
      if (st !== mine || !info) return;
      mine.info = info;
      if (!mine.form.resource) mine.form = newFoundryForm(info);
      ctx.rerender();
    })
    .catch((e) => {
      if (st !== mine) return;
      mine.error = errorText(e);
      ctx.rerender();
    });
}

/** The open sheet, if any (the page appends it). */
export function foundryDialog(ctx: Ctx): HTMLElement | null {
  const s = st;
  if (!s) return null;
  const t = ctx.snap.teams.find((x) => x.id === s.team);
  if (!t) {
    st = null;
    return null;
  }
  const close = () => {
    st = null;
    ctx.rerender();
  };
  return s.result ? resultSheet(ctx, s, s.result, close) : formSheet(ctx, t, s, close);
}

function formSheet(ctx: Ctx, t: Team, s: State, close: () => void): HTMLElement {
  const f = s.form;
  const errs = s.tried ? foundryErrors(f) : {};
  const save = button("Save and check", { variant: "primary", id: "foundry-save", loading: s.busy, onclick: () => void submit(ctx, s) });
  const input = (o: { id: string; key: keyof FoundryForm; label: string; hint?: string; secret?: boolean; placeholder?: string }) =>
    field({
      id: o.id,
      label: o.label,
      value: f[o.key],
      type: o.secret ? "password" : "text",
      placeholder: o.placeholder,
      hint: o.hint,
      error: errs[o.key as keyof typeof errs] ?? null,
      disabled: s.busy,
      oninput: (v) => ((f[o.key] as string) = v),
      onenter: () => void submit(ctx, s),
    });
  const resource = input({ id: "foundry-resource", key: "resource", label: "Resource name", placeholder: "my-resource", hint: "The name only, not the URL." });
  const auth = select({
    id: "foundry-auth",
    label: "How Kivali signs in to Azure",
    options: [
      { value: "api_key", label: "API key" },
      { value: "service_principal", label: "Service principal" },
    ],
    value: f.auth,
    disabled: s.busy,
    onchange: (v) => {
      f.auth = v === "service_principal" ? "service_principal" : "api_key";
      ctx.rerender();
    },
  });
  const how: Child[] =
    f.auth === "api_key"
      ? [input({ id: "foundry-api-key", key: "apiKey", label: "API key", secret: true, hint: "In the Foundry portal, under Endpoints and keys." }).wrap]
      : [
          input({ id: "foundry-tenant", key: "tenantId", label: "Tenant ID" }).wrap,
          input({ id: "foundry-client", key: "clientId", label: "Client ID" }).wrap,
          input({ id: "foundry-secret", key: "clientSecret", label: "Client secret", secret: true }).wrap,
          caption("The service principal needs only the Azure AI User role on this resource."),
        ];
  const models = s.info?.models ?? [];
  const names: Child[] = models.flatMap((m, i) => [i ? ", " : "", h("span", { class: "mono-path" }, m)]);
  return sheet({
    key: `foundry-${t.id}`,
    title: "Use Microsoft Foundry",
    body: [
      p(`Claude’s calls from ${t.name} go to your Foundry resource, and Azure bills them.`),
      resource.wrap,
      auth,
      ...how,
      models.length ? caption(h("span", {}, "Name each deployment after the model it runs: ", ...names, ".")) : null,
      caption(`Kivali saves these in Claude Code’s settings in ${t.name}.`),
      s.error ? banner("danger", "Couldn’t save", s.error) : null,
      s.busy ? caption("Checking each deployment. This can take a minute.") : null,
    ].filter((c): c is HTMLElement => !!c),
    buttons: [button("Cancel", { variant: "ghost", id: "foundry-cancel", onclick: close }), save],
    focus: resource.input,
    onCancel: close,
  });
}

async function submit(ctx: Ctx, s: State) {
  if (s.busy) return;
  s.tried = true;
  s.error = null;
  if (Object.keys(foundryErrors(s.form)).length) {
    ctx.rerender();
    return;
  }
  s.busy = true;
  ctx.rerender();
  const { setup, values } = foundrySetup(s.form);
  try {
    const r = await ctx.act<SetupResult>("apply_credential_setup", { id: s.team, setup, values }, true);
    if (r) s.result = r;
  } catch (e) {
    s.error = errorText(e);
  }
  s.busy = false;
  // The secrets leave page memory once sent.
  s.form.apiKey = "";
  s.form.clientSecret = "";
  if (st === s) ctx.rerender();
}

function resultSheet(ctx: Ctx, s: State, r: SetupResult, close: () => void): HTMLElement {
  const o = foundryOutcome(r);
  const list = (items: string[]) => h("ul", { class: "sheet-list" }, ...items.map((x) => h("li", {}, x)));
  const body: Child[] = [
    o.signedIn
      ? banner("success", "Signed in to Microsoft Foundry", o.billing ? `Billing: ${o.billing}.` : null)
      : banner("danger", "Claude isn’t signed in", "Claude Code didn’t take these settings. Check them and try again."),
  ];
  if (o.missing.length) body.push(banner("warning", missingTitle(o.missing.length), "Agents that use these models can’t work until you add them."), list(o.missing));
  if (o.failed.length) body.push(banner("danger", "Some deployments didn’t answer", null), list(o.failed));
  if (!o.missing.length && !o.failed.length && o.ok.length) body.push(p(o.ok.length === 1 ? "Its deployment answered." : `All ${o.ok.length} deployments answered.`));
  const done = button("Done", { variant: "primary", id: "foundry-done", onclick: close });
  return sheet({
    key: `foundry-${s.team}-result`,
    title: "Use Microsoft Foundry",
    body: body.filter((c): c is HTMLElement => !!c),
    buttons: [
      button("Change settings", {
        variant: "ghost",
        id: "foundry-change",
        onclick: () => {
          s.result = null;
          s.tried = false;
          ctx.rerender();
        },
      }),
      done,
    ],
    defaultButton: done,
    onCancel: close,
  });
}
