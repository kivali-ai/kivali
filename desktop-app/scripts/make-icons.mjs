// Renders Kivali Desktop's tray icons with rsvg-convert.
//
//   node scripts/make-icons.mjs      (or `make icons`)
//
// Four states, drawn from the logo's bars and dots (viewBox 11 3 78 88,
// the design's 20x22 pt icon):
//
//   running        solid bars and dots, full strength
//   paused         outlined, at 45%
//   starting       the paused drawing pulsing 45% -> 100% -> 45% over
//                  1.6 s: FRAMES frames, plus a held frame at 50% for
//                  Reduce motion
//   couldnt-start  solid bars, red dots
//
// Each in two inks, for light and dark menu bars (#1D1D1F, #F5F5F5).
// They are coloured images, never template images, so the red survives.
//
//   tray-<state>-<light|dark>.png             40x44 (@2x of 20x22 pt), the macOS menu bar
//   tray-starting-<light|dark>-<i>.png        the pulse frames, i = 0..FRAMES-1
//   tray-win-…                                the same names at 32x32, the Windows
//                                             notification area (dark taskbar: -dark)
//
// platform/mod.rs (`tray_set!`) includes them by these names; trayicon.rs
// picks one. The output is committed; rerun only when the drawing changes.
// The app icon is not drawn here: `make icons` renders the official
// Kivali icon onto the macOS canvas and hands it to `tauri icon`.

import { execFileSync } from "node:child_process";
import { writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const here = dirname(fileURLToPath(import.meta.url));
const out = join(here, "..", "src-tauri", "icons");

// Keep in step with platform::TRAY_FRAMES and TRAY_FRAME_MS.
const FRAMES = 10;
const PERIOD_MS = 1600;

const INK = { light: "#1D1D1F", dark: "#F5F5F5" };
const DANGER = { light: "#B3261E", dark: "#F2877D" };

const solid = (ink, dot) =>
  `<rect x="26" y="48" width="16" height="40" rx="8" fill="${ink}"/>` +
  `<rect x="58" y="58.4" width="16" height="29.6" rx="8" fill="${ink}"/>` +
  `<circle cx="34" cy="25.8" r="11.8" fill="${dot}"/>` +
  `<circle cx="66" cy="36.2" r="11.8" fill="${dot}"/>`;

const outline = (ink) =>
  `<rect x="29.5" y="51.5" width="9" height="33" rx="4.5" fill="none" stroke="${ink}" stroke-width="7"/>` +
  `<rect x="61.5" y="61.9" width="9" height="22.6" rx="4.5" fill="none" stroke="${ink}" stroke-width="7"/>` +
  `<circle cx="34" cy="25.8" r="8.3" fill="none" stroke="${ink}" stroke-width="7"/>` +
  `<circle cx="66" cy="36.2" r="8.3" fill="none" stroke="${ink}" stroke-width="7"/>`;

// Two canvases: the design's 20:22 for the menu bar, and a square one
// (the same drawing centred, viewBox widened) for the notification area.
const CANVASES = {
  tray: { w: 40, h: 44, viewBox: "11 3 78 88" },
  "tray-win": { w: 32, h: 32, viewBox: "6 3 88 88" },
};

function svg(canvas, body, opacity) {
  return (
    `<svg xmlns="http://www.w3.org/2000/svg" width="${canvas.w}" height="${canvas.h}" viewBox="${canvas.viewBox}">` +
    `<g opacity="${opacity.toFixed(4)}">${body}</g></svg>`
  );
}

function render(name, canvas, body, opacity) {
  const png = execFileSync("rsvg-convert", ["-w", String(canvas.w), "-h", String(canvas.h), "-f", "png"], {
    input: svg(canvas, body, opacity),
  });
  writeFileSync(join(out, name), png);
  console.log(`wrote ${name} (${canvas.w}x${canvas.h}, ${png.length} bytes)`);
}

// The pulse: 45% at t=0, 100% at half the period, eased (cosine).
const pulse = (i) => 0.45 + 0.55 * ((1 - Math.cos((2 * Math.PI * i) / FRAMES)) / 2);

for (const [prefix, canvas] of Object.entries(CANVASES)) {
  for (const tone of ["light", "dark"]) {
    const ink = INK[tone];
    render(`${prefix}-running-${tone}.png`, canvas, solid(ink, ink), 1);
    render(`${prefix}-paused-${tone}.png`, canvas, outline(ink), 0.45);
    render(`${prefix}-couldnt-start-${tone}.png`, canvas, solid(ink, DANGER[tone]), 1);
    render(`${prefix}-starting-${tone}.png`, canvas, outline(ink), 0.5);
    for (let i = 0; i < FRAMES; i++) {
      render(`${prefix}-starting-${tone}-${i}.png`, canvas, outline(ink), pulse(i));
    }
  }
}
console.log(`starting: ${FRAMES} frames, ${PERIOD_MS / FRAMES} ms each`);
