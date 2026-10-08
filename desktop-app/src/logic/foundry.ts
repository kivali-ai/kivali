// The Microsoft Foundry form: what it checks before sending, the
// sign-in setup it sends (apply_credential_setup), and the words for
// what came back. The page is pages/foundry.ts; the Claude driver
// behind the supervisor checks the values again and owns what they
// become.

import type { SetupInfo, SetupResult } from "../types";

/** The Claude driver's id for this setup. */
export const FOUNDRY_SETUP = "microsoft-foundry";

export type FoundryAuth = "api_key" | "service_principal";

export interface FoundryForm {
  resource: string;
  auth: FoundryAuth;
  apiKey: string;
  tenantId: string;
  clientId: string;
  clientSecret: string;
}

export type FoundryField = "resource" | "apiKey" | "tenantId" | "clientId" | "clientSecret";

/** A fresh form, starting from the saved setup when it is this one (never its secrets). */
export function newFoundryForm(info?: SetupInfo | null): FoundryForm {
  const saved = info?.current?.setup === FOUNDRY_SETUP ? info.current.values : {};
  return {
    resource: saved.resource ?? "",
    auth: saved.auth === "service_principal" ? "service_principal" : "api_key",
    apiKey: "",
    tenantId: saved.tenant_id ?? "",
    clientId: saved.client_id ?? "",
    clientSecret: "",
  };
}

/** An Azure resource name: letters, digits and hyphens, not starting or ending with a hyphen. */
const RESOURCE = /^[A-Za-z0-9](?:[A-Za-z0-9-]{0,62}[A-Za-z0-9])?$/;

/** "my-resource" from a pasted endpoint like https://my-resource.services.ai.azure.com/anthropic. */
export function resourceFromEndpoint(v: string): string | null {
  const m = /^(?:https?:\/\/)?([A-Za-z0-9-]+)\.services\.ai\.azure\.com(?:[:/].*)?$/i.exec(v.trim());
  return m && RESOURCE.test(m[1]) ? m[1] : null;
}

/** What is wrong with each field, in the form's words; empty when it can be sent. */
export function foundryErrors(f: FoundryForm): Partial<Record<FoundryField, string>> {
  const out: Partial<Record<FoundryField, string>> = {};
  const resource = f.resource.trim();
  if (!resource) out.resource = "Enter the resource name.";
  else if (!RESOURCE.test(resource)) {
    const name = resourceFromEndpoint(resource);
    out.resource = name ? `Enter the name only: ${name}.` : "Enter the name only, like my-resource. Letters, numbers and hyphens.";
  }
  const need = (k: FoundryField, label: string) => {
    const v = f[k].trim();
    if (!v) out[k] = `Enter the ${label}.`;
    else if (/\s/.test(v)) out[k] = `The ${label} can’t have spaces.`;
  };
  if (f.auth === "api_key") need("apiKey", "API key");
  else {
    need("tenantId", "tenant ID");
    need("clientId", "client ID");
    need("clientSecret", "client secret");
  }
  return out;
}

/** The apply_credential_setup arguments: this setup, and the chosen way's values, trimmed. */
export function foundrySetup(f: FoundryForm): { setup: string; values: Record<string, string> } {
  const resource = f.resource.trim();
  const values: Record<string, string> =
    f.auth === "api_key"
      ? { resource, auth: f.auth, api_key: f.apiKey.trim() }
      : { resource, auth: f.auth, tenant_id: f.tenantId.trim(), client_id: f.clientId.trim(), client_secret: f.clientSecret.trim() };
  return { setup: FOUNDRY_SETUP, values };
}

export interface FoundryOutcome {
  signedIn: boolean;
  /** "Microsoft Foundry · my-resource". */
  billing: string | null;
  /** "No deployment named claude-sonnet-5 in my-resource", one per model. */
  missing: string[];
  /** "claude-opus-5-5: the provider refused the sign-in (HTTP 401)". */
  failed: string[];
  /** Models that answered. */
  ok: string[];
}

export function foundryOutcome(r: SetupResult): FoundryOutcome {
  return {
    signedIn: r.credential.signed_in,
    billing: r.credential.billing ?? null,
    missing: r.models.filter((m) => !m.ok && m.missing).map((m) => m.problem ?? `No deployment named ${m.model}`),
    failed: r.models.filter((m) => !m.ok && !m.missing).map((m) => `${m.model}: ${m.problem ?? "no answer"}`),
    ok: r.models.filter((m) => m.ok).map((m) => m.model),
  };
}

/** "1 deployment is missing" / "2 deployments are missing". */
export function missingTitle(n: number): string {
  return n === 1 ? "1 deployment is missing" : `${n} deployments are missing`;
}

/** Whether a team signs in through Microsoft Foundry now, from its billing words. */
export function isFoundry(billing: string | null | undefined): boolean {
  return !!billing && billing.startsWith("Microsoft Foundry");
}
