// What to tell the person when this computer can't run a team's virtual
// machine (the shell's `vm_support`): the welcome screen, a team's page
// and setup's failure screen show it. Pure (vmhelp.test.ts).

import type { VmSupport } from "../types";

export interface VmHelp {
  title: string;
  body: string;
  /** Numbered steps; empty when there is nothing to do on this computer. */
  steps: string[];
  /** A line under the steps, for the case the steps don't cover. */
  note: string | null;
}

/** Null when teams can run here. */
export function vmHelp(v: VmSupport): VmHelp | null {
  switch (v) {
    case "ready":
      return null;
    case "hyperv_off":
      return {
        title: "Turn on Hyper-V to run a team on this PC",
        body: "Kivali runs each team in a Hyper-V virtual machine, and Hyper-V is turned off. Turning it on takes an administrator and a restart.",
        steps: [
          "Open Start, type “Turn Windows features on or off” and open it.",
          "Select Hyper-V, then OK.",
          "Restart this PC when Windows asks.",
          "Open Kivali again.",
        ],
        note: "Turned it on already? Restart this PC to finish.",
      };
    case "windows_home":
      return {
        title: "Windows Home can’t run a team on this PC",
        body: "Kivali runs each team in a Hyper-V virtual machine. Hyper-V comes with Windows Pro, Enterprise and Education, not Home.",
        steps: [
          "Open Settings, search for “Activation” and open it.",
          "Under Upgrade your edition of Windows, upgrade to Windows Pro.",
          "Open Start, type “Turn Windows features on or off”, select Hyper-V, then OK.",
          "Restart this PC, then open Kivali again.",
        ],
        note: null,
      };
    case "virtualization_off":
      return {
        title: "Turn on virtualization to run a team on this PC",
        body: "Hyper-V is on, but it can’t run because virtualization is turned off in this PC’s firmware.",
        steps: [
          "Hold Shift while you choose Restart from the Start menu.",
          "Choose Troubleshoot, Advanced options, UEFI Firmware Settings, then Restart.",
          "Turn on the setting called Intel Virtualization Technology, VT-x, AMD-V or SVM Mode. Save and exit.",
          "Open Kivali again.",
        ],
        note: "Already on in the firmware? In a terminal opened as administrator, run bcdedit /set hypervisorlaunchtype auto, then restart.",
      };
    case "in_vm":
      return {
        title: "This Mac can’t run a team",
        body: "Kivali runs each team in a virtual machine, and this Mac can’t start one. This happens when macOS itself runs in a virtual machine.",
        steps: [],
        note: "Create the team on a Mac that runs macOS directly.",
      };
  }
}
