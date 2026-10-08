// The layout breakpoints from the design, for code that must branch on width (a component variant, a count).
// CSS uses the same numbers in @media params, where custom properties do not work. This file is the one
// place TypeScript may spell them in px (lint/syntax-rules.json exempts it).

/** Below this the sidebar gives way to the phone header and tab bar. */
export const BREAKPOINT_TABBAR = 960;

/** Below this grids collapse to one column. */
export const BREAKPOINT_ONE_COLUMN = 640;

/** A min-width media query for `window.matchMedia`: true at and above `width`. */
export function mediaQuery(width: number): string {
  return `(min-width: ${width}px)`;
}
