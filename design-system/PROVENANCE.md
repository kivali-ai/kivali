# Provenance

This folder is the Kivali Design System, vendored verbatim from the design system's own
export. Do not edit files here by hand: changes are made in the design system and brought
in as a new export.

To bring in a new export:

1. `scripts/ds-import.sh <zip>` unpacks it into `design-system/`, dropping the export's own
   `uploads/` and thumbnail files and keeping Kivali's own files (below). `--dry-run` lists
   what would change.
   Review the diff.
2. `make ds-sync` copies the CSS, tokens, fonts and logos into `web/src/ds/` and
   `web/public/logos/`, which is what the app imports and serves.
3. Update the TSX ports in `web/src/ds/` where the reference `.jsx` changed, then run
   `make web-lint web-test`.

Two files here are Kivali's own: this one and `components/text.css` (type classes for the
Text component). The import keeps both.

`_ds_bundle.js` is the built bundle of the reference components. It is kept so the
`.card.html` specimens and `ui_kits/app/index.html` render when served over HTTP. The app
never loads it.
