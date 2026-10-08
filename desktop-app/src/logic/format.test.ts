import { describe, expect, it } from "vitest";
import {
  ago,
  deviceName,
  deviceShort,
  formatBytes,
  formatMemory,
  initials,
  normalizeAddress,
  parseDevice,
  shortVersion,
  sinceWhen,
  tildePath,
} from "./format";

// Local-time dates, so the tests read the same in every time zone.
const at = (d: number, h: number, m: number) => new Date(2026, 9, d, h, m);

describe("sizes", () => {
  it("memory", () => {
    expect(formatMemory(4096)).toBe("4 GB");
    expect(formatMemory(6144)).toBe("6 GB");
    expect(formatMemory(1536)).toBe("1.5 GB");
    expect(formatMemory(16384)).toBe("16 GB");
    expect(formatMemory(512)).toBe("512 MB");
  });
  it("disk", () => {
    expect(formatBytes(18 * 1024 ** 3)).toBe("18 GB");
    expect(formatBytes(2.4 * 1024 ** 3)).toBe("2.4 GB");
    expect(formatBytes(640 * 1024 ** 2)).toBe("640 MB");
  });
});

describe("versions", () => {
  it("drops a trailing .0 and a v", () => {
    expect(shortVersion("0.17.0")).toBe("0.17");
    expect(shortVersion("v0.16.2")).toBe("0.16.2");
    expect(shortVersion(null)).toBeNull();
  });
});

describe("sinceWhen", () => {
  const now = at(5, 15, 0);
  it("reads like the app writes dates", () => {
    expect(sinceWhen(at(5, 9, 14).toISOString(), now)).toBe("9:14 this morning");
    expect(sinceWhen(at(5, 13, 5).toISOString(), now)).toBe("1:05 this afternoon");
    expect(sinceWhen(at(4, 18, 30).toISOString(), now)).toBe("6:30 yesterday evening");
    expect(sinceWhen(at(2, 23, 0).toISOString(), now)).toBe("11:00 Friday night");
    expect(sinceWhen(at(1, 8, 0).toISOString(), at(20, 8, 0))).toBe("1 Oct");
    expect(sinceWhen("nonsense", now)).toBeNull();
  });
});

describe("ago", () => {
  const now = at(5, 15, 0);
  it("counts back", () => {
    expect(ago(at(5, 14, 59).toISOString(), now)).toBe("1 minute ago");
    expect(ago(at(5, 13, 0).toISOString(), now)).toBe("2 hours ago");
    expect(ago(at(4, 10, 0).toISOString(), now)).toBe("yesterday");
    expect(ago(at(1, 10, 0).toISOString(), now)).toBe("4 days ago");
    expect(ago(new Date(2026, 8, 12).toISOString(), now)).toBe("12 Sep");
    expect(ago(now.toISOString(), now)).toBe("just now");
  });
});

describe("devices", () => {
  it("names a computer from its hostname", () => {
    expect(deviceName("dana-imac.local")).toBe("Dana’s iMac");
    expect(deviceShort("dana-imac.local")).toBe("iMac");
    expect(deviceName("Danas-MacBook-Pro.local")).toBe("Dana’s MacBook Pro");
    expect(deviceName("james-mac-mini")).toBe("James’s Mac mini");
    expect(deviceName("imac.lan")).toBe("the iMac");
    expect(deviceName("kivali.example.com")).toBe("kivali.example.com");
    expect(deviceShort("studio-server")).toBe("studio-server");
    expect(deviceName(null)).toBe("another computer");
    expect(parseDevice("https://dana-imac.local:8443").model).toBe("iMac");
  });
});

describe("small words", () => {
  it("initials", () => {
    expect(initials("Dana Parker")).toBe("DP");
    expect(initials("dana@example.com")).toBe("DA");
    expect(initials("dana.parker@x.com")).toBe("DP");
  });
  it("tilde paths", () => {
    expect(tildePath("/Users/dana/Library/Application Support/Kivali")).toBe("~/Library/Application Support/Kivali");
    expect(tildePath("C:\\Users\\dana\\AppData")).toBe("C:\\Users\\dana\\AppData");
  });
  it("adds https:// to a plain hostname", () => {
    expect(normalizeAddress("dana-imac.local:8443")).toBe("https://dana-imac.local:8443");
    expect(normalizeAddress(" https://x.example ")).toBe("https://x.example");
    expect(normalizeAddress("http://10.0.0.2")).toBe("http://10.0.0.2");
    expect(normalizeAddress("")).toBe("");
  });
});
