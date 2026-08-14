# Vendored webfonts

These are served by the app's own origin. Nothing here is fetched from a CDN —
Triage Factory runs self-hosted and sometimes air-gapped, so a remote font
request would fail in exactly the deployments that matter most, and would be a
third-party call from a product that makes none.

| File | Family | Weights | Subset |
| --- | --- | --- | --- |
| `archivo-latin.woff2` | Archivo | 400–700 (variable) | latin |
| `archivo-latin-ext.woff2` | Archivo | 400–700 (variable) | latin-ext |
| `dmmono-{300,400,500}-latin.woff2` | DM Mono | 300 / 400 / 500 | latin |
| `dmmono-{300,400,500}-latin-ext.woff2` | DM Mono | 300 / 400 / 500 | latin-ext |

Archivo is variable, so one file per subset covers the whole 400–700 range.
DM Mono is not, so each weight is its own file. The `@font-face` rules and the
`unicode-range` declarations that keep `latin-ext` from loading on pages that
don't need it live in `frontend/src/fonts.css`.

## Licence

Both families are licensed under the **SIL Open Font License, Version 1.1**,
which permits bundling and redistribution with the reserved-name and
same-licence conditions. Neither family is renamed here.

- Archivo — Copyright 2017 The Archivo Project Authors
  (https://github.com/Omnibus-Type/Archivo)
- DM Mono — Copyright 2020 The DM Mono Project Authors
  (https://github.com/googlefonts/dm-mono)

Full licence text: https://openfontlicense.org/

## Updating

Fetch the `woff2` URLs from the Google Fonts CSS API with a modern browser
User-Agent (an older UA is served `ttf` instead), then re-copy the
`unicode-range` values into `frontend/src/fonts.css` — they are versioned
alongside the binaries and drift if only one side is updated.
