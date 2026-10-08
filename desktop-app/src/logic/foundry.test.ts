import { describe, expect, it } from "vitest";
import type { SetupInfo, SetupResult } from "../types";
import { FOUNDRY_SETUP, foundryErrors, foundryOutcome, foundrySetup, isFoundry, missingTitle, newFoundryForm, resourceFromEndpoint } from "./foundry";

describe("the Microsoft Foundry form", () => {
  it("starts from the saved setup, never its secrets", () => {
    expect(newFoundryForm()).toEqual({ resource: "", auth: "api_key", apiKey: "", tenantId: "", clientId: "", clientSecret: "" });
    const info: SetupInfo = { models: [], current: { setup: FOUNDRY_SETUP, values: { resource: "r", auth: "service_principal", tenant_id: "t", client_id: "c" } } };
    expect(newFoundryForm(info)).toEqual({ resource: "r", auth: "service_principal", apiKey: "", tenantId: "t", clientId: "c", clientSecret: "" });
    expect(newFoundryForm({ models: [], current: { setup: "other", values: { resource: "x" } } }).resource).toBe("");
  });

  it("wants a bare resource name and the chosen way's values", () => {
    const f = { ...newFoundryForm(), resource: "my-resource", apiKey: "k" };
    expect(foundryErrors(f)).toEqual({});
    expect(foundryErrors({ ...f, resource: "" }).resource).toBe("Enter the resource name.");
    expect(foundryErrors({ ...f, resource: "https://my-resource.services.ai.azure.com/anthropic" }).resource).toBe("Enter the name only: my-resource.");
    expect(foundryErrors({ ...f, resource: "my_resource" }).resource).toMatch(/^Enter the name only, like my-resource/);
    expect(foundryErrors({ ...f, resource: "-r" }).resource).toBeTruthy();
    expect(foundryErrors({ ...f, apiKey: " " }).apiKey).toBe("Enter the API key.");
    expect(foundryErrors({ ...f, apiKey: "a b" }).apiKey).toBe("The API key can’t have spaces.");
    const sp = { ...f, auth: "service_principal" as const, apiKey: "" };
    expect(foundryErrors(sp)).toEqual({ tenantId: "Enter the tenant ID.", clientId: "Enter the client ID.", clientSecret: "Enter the client secret." });
    expect(foundryErrors({ ...sp, tenantId: "t", clientId: "c", clientSecret: "s" })).toEqual({});
  });

  it("reads a pasted endpoint's resource", () => {
    expect(resourceFromEndpoint("my-resource.services.ai.azure.com")).toBe("my-resource");
    expect(resourceFromEndpoint("https://x.example.com")).toBeNull();
  });

  // The body the supervisor's SetupRequest reads (internal/supervisor,
  // TestSetupRPC): the setup id and only the chosen way's values.
  it("sends a sign-in setup with the chosen way's values only", () => {
    const f = { ...newFoundryForm(), resource: " my-resource ", apiKey: " k ", tenantId: "t", clientSecret: "left over" };
    expect(foundrySetup(f)).toEqual({ setup: "microsoft-foundry", values: { resource: "my-resource", auth: "api_key", api_key: "k" } });
    expect(foundrySetup({ ...f, auth: "service_principal", clientId: "c" })).toEqual({
      setup: "microsoft-foundry",
      values: { resource: "my-resource", auth: "service_principal", tenant_id: "t", client_id: "c", client_secret: "left over" },
    });
  });

  // The JSON internal/supervisor's TestSetupWireShapes asserts the Go side emits.
  it("reads the setup result the supervisor sends", () => {
    const r: SetupResult = JSON.parse(
      `{"credential":{"signed_in":true,"billing":"P · r","checked_at":"2026-10-01T12:00:00Z"},"models":[{"model":"a","ok":true},{"model":"b","ok":false,"missing":true,"problem":"No b in r"},{"model":"c","ok":false,"problem":"refused"}]}`,
    );
    expect(foundryOutcome(r)).toEqual({ signedIn: true, billing: "P · r", missing: ["No b in r"], failed: ["c: refused"], ok: ["a"] });
  });

  it("names the result", () => {
    expect(missingTitle(1)).toBe("1 deployment is missing");
    expect(missingTitle(3)).toBe("3 deployments are missing");
    expect(isFoundry("Microsoft Foundry · r")).toBe(true);
    expect(isFoundry("Claude Max")).toBe(false);
    expect(isFoundry(null)).toBe(false);
  });
});
