// App CSS rules. Vendored files (src/ds/kivali.css,
// src/ds/tokens/**, src/ds/fonts/**) are excluded by run.mjs, never linted.

const STRICT_PROPS = [
  "/^color$/",
  "background",
  "background-color",
  "/^border(-(top|right|bottom|left))?$/",
  "/^border(-(top|right|bottom|left))?-color$/",
  "/^border(-(top|right|bottom|left))?-width$/",
  "outline",
  "outline-color",
  "fill",
  "stroke",
  // `font` is allowed only as a token (the design system ships shorthands such as --text-body).
  "font",
  "font-family",
  "font-size",
  "line-height",
  "letter-spacing",
  "/^border(-(top|bottom)-(left|right|start|end))?-radius$/",
  "box-shadow",
  "/^padding/",
  "/^margin/",
  "gap",
  "row-gap",
  "column-gap",
  "/^inset/",
  "top",
  "right",
  "bottom",
  "left",
  "width",
  "height",
  "/^min-(width|height)$/",
  "/^max-(width|height)$/",
  "z-index",
  "transition-duration",
  "transition-timing-function",
  "animation-duration",
  "animation-timing-function",
];

// Arithmetic over tokens: calc/min/max/clamp whose only numbers are unitless, %, 0 or 1px, so
// `calc(var(--space-4) * -1)` passes and `calc(100% - 12px)` does not.
const TOKEN_MATH = "/^(?!.*(?<![\\w-])(?!(?:1px|100d?vh)\\b)\\d*\\.?\\d+(?:px|r?em|pt|ch|ex|vw|vh|dvh|svh|lvh|vmin|vmax|m?s)\\b)(?:calc|min|max|clamp)\\(.*\\)$/";

// Literals allowed where a token would otherwise be required.
const LITERALS = [
  "0",
  "1px",
  // The hairline tuck: a row pulls up by its own 1px border so two hairlines never stack. No other negative px.
  "-1px",
  "auto",
  "inherit",
  "initial",
  "currentColor",
  "currentcolor",
  "transparent",
  "none",
  "solid",
  "dashed",
  "dotted",
  "100vh",
  "100dvh",
  "/^-?\\d*\\.?\\d+%$/",
  "/^-?\\d*\\.?\\d+fr$/",
  TOKEN_MATH,
];

export default {
  plugins: ["stylelint-declaration-strict-value"],
  rules: {
    "scale-unlimited/declaration-strict-value": [
      STRICT_PROPS,
      {
        ignoreValues: LITERALS,
        ignoreFunctions: false,
        disableFix: true,
        message:
          "Use a design-system token: var(--…) for ${property} (got ${value}). Allowed literals: 0, 1px, -1px, auto, inherit, currentColor, transparent, none, %, fr, 100vh/100dvh.",
      },
    ],
    "color-no-hex": true,
    "declaration-no-important": true,
    "font-family-no-missing-generic-family-keyword": null,
    // Shorthands the strict-value rule does not see: no raw durations or easings.
    "declaration-property-value-disallowed-list": [
      {
        "/^(transition|animation)/": [
          "/\\b\\d*\\.?[1-9]\\d*m?s\\b/",
          "/(?<![\\w-])(ease|ease-in|ease-out|ease-in-out|linear|step-start|step-end|cubic-bezier|steps)(?![\\w-])/",
        ],
      },
      { message: "Raw duration or easing in transition/animation. Use var(--duration-*) and var(--ease-*)." },
    ],
    "selector-class-pattern": [
      "^(app-[a-z0-9]+(-[a-z0-9]+)*|(is|has)-[a-z0-9]+(-[a-z0-9]+)*)$",
      {
        resolveNestedSelectors: false,
        message: (name) => `Class ${name} must be prefixed app- (state modifiers is-*/has-* only compounded with an app class).`,
      },
    ],
    "selector-disallowed-list": [
      [
        "/\\.kv-/",
        // a state modifier that begins a compound (not attached to an app- class)
        "/(^|[\\s>+~,])\\.(is|has)-/",
      ],
      {
        message:
          "App CSS never restyles a design-system .kv- class, and is-*/has-* modifiers must be compounded with an app- class.",
      },
    ],
    "at-rule-disallowed-list": [
      ["font-face"],
      { message: "@font-face lives in the vendored src/ds/tokens/typography.css only." },
    ],
  },
  overrides: [
    {
      // Kivali's own additions under src/ds (e.g. src/ds/text.css for the Text component). The
      // token rules above still apply; the class rules flip: they define .kv-* classes in the
      // kivali.css shape (kv-block-elem--modifier, plus is-*/has-* state) and never touch app- classes.
      // Globs resolve against this file's directory (web/lint): ../src/ds is the real tree,
      // fixtures/*/src/ds the fixture sets.
      files: ["../src/ds/**/*.css", "fixtures/*/src/ds/**/*.css"],
      rules: {
        "selector-class-pattern": [
          "^(kv-[a-z0-9]+(-[a-z0-9]+)*(--[a-z0-9]+(-[a-z0-9]+)*)?|(is|has)-[a-z0-9]+(-[a-z0-9]+)*)$",
          {
            resolveNestedSelectors: false,
            message: (name) => `Class ${name} in src/ds must be kv-block[-elem][--modifier] or an is-*/has-* state.`,
          },
        ],
        "selector-disallowed-list": [
          ["/\\.app-/", "/(^|[\\s>+~,])\\.(is|has)-/"],
          {
            message: "Design-system CSS never styles app- classes, and is-*/has-* modifiers must be compounded with a kv- class.",
          },
        ],
      },
    },
  ],
};
