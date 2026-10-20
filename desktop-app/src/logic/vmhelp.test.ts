import { describe, expect, it } from "vitest";
import type { VmSupport } from "../types";
import { vmHelp } from "./vmhelp";

const BLOCKED: VmSupport[] = ["hyperv_off", "windows_home", "virtualization_off", "in_vm"];

describe("vmHelp", () => {
  it("says nothing when teams can run here", () => {
    expect(vmHelp("ready")).toBeNull();
  });

  it("says what to do for each way this computer can't run a team", () => {
    expect(vmHelp("hyperv_off")?.steps[0]).toBe("Open Start, type “Turn Windows features on or off” and open it.");
    expect(vmHelp("hyperv_off")?.note).toBe("Turned it on already? Restart this PC to finish.");
    expect(vmHelp("windows_home")?.body).toContain("not Home");
    expect(vmHelp("virtualization_off")?.steps.join(" ")).toContain("UEFI Firmware Settings");
    expect(vmHelp("virtualization_off")?.note).toContain("bcdedit /set hypervisorlaunchtype auto");
    // Nothing to do on this Mac itself: no steps, a line on where to run it.
    expect(vmHelp("in_vm")?.steps).toEqual([]);
    expect(vmHelp("in_vm")?.title).toBe("This Mac can’t run a team");
  });

  it("follows the voice: sentence case, full sentences, no exclamation marks", () => {
    for (const v of BLOCKED) {
      const h = vmHelp(v)!;
      for (const line of [h.title, h.body, ...h.steps, ...(h.note ? [h.note] : [])]) {
        expect(line).not.toMatch(/!/);
        expect(line[0]).toBe(line[0].toUpperCase());
      }
      expect(h.title).not.toMatch(/\.$/);
      for (const line of [h.body, ...h.steps, ...(h.note ? [h.note] : [])]) expect(line).toMatch(/[.?]$/);
    }
  });
});
