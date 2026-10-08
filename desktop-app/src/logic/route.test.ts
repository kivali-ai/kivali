import { describe, expect, it } from "vitest";
import { parseRoute, routeHash, setupTitle } from "./route";

describe("parseRoute", () => {
  it("reads every window's routes", () => {
    expect(parseRoute("#/setup/welcome")).toEqual({ page: "setup", mode: "welcome" });
    expect(parseRoute("#/setup/add")).toEqual({ page: "setup", mode: "add" });
    expect(parseRoute("#/setup/new")).toEqual({ page: "setup", mode: "new" });
    expect(parseRoute("#/connect")).toEqual({ page: "connect" });
    expect(parseRoute("#/settings/general")).toEqual({ page: "settings", section: "general" });
    expect(parseRoute("#/settings/advanced")).toEqual({ page: "settings", section: "advanced" });
    expect(parseRoute("#/settings/team/abc")).toEqual({ page: "settings", section: "team", id: "abc", tab: "overview" });
    expect(parseRoute("#/settings/team/abc/ai")).toEqual({ page: "settings", section: "team", id: "abc", tab: "ai" });
    expect(parseRoute("#/settings/team/abc/devices")).toEqual({ page: "settings", section: "team", id: "abc", tab: "devices" });
    expect(parseRoute("#/settings/team/abc/nope")).toEqual({ page: "settings", section: "team", id: "abc", tab: "overview" });
    expect(parseRoute("#/team/home")).toEqual({ page: "team", id: "home" });
  });
  it("falls back to the welcome screen", () => {
    expect(parseRoute("")).toEqual({ page: "setup", mode: "welcome" });
    expect(parseRoute("#/nope")).toEqual({ page: "setup", mode: "welcome" });
    expect(parseRoute("#/settings")).toEqual({ page: "settings", section: "general" });
  });
  it("round-trips", () => {
    for (const h of ["#/setup/new", "#/connect", "#/settings/team/x%20y/mac", "#/team/home", "#/settings/advanced"])
      expect(routeHash(parseRoute(h))).toBe(h);
  });
  it("titles the setup window", () => {
    expect(setupTitle(parseRoute("#/setup/welcome"))).toBe("Welcome to Kivali");
    expect(setupTitle(parseRoute("#/setup/add"))).toBe("Add a team");
    expect(setupTitle(parseRoute("#/setup/new"))).toBe("New team");
    expect(setupTitle(parseRoute("#/connect"))).toBe("Connect to a team");
    expect(setupTitle(parseRoute("#/team/x"))).toBeNull();
  });
});
