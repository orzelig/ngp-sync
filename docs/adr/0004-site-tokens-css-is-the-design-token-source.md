# 0004. site/tokens.css is the design-token source

- **Status:** accepted
- **Date:** 2026-11-25

## Context

ngp.one's three pages each carried their own inline `<style>`, light with a
`prefers-color-scheme: dark` override, an orange gradient button and a few
transitions. NGP Phone's design3 language (always dark, glass surfaces, one
accent, the orb as the only working indicator) is meant to be shared by the
phone, the browser and the site. That needs its values in one place that other
products can later read, and that cannot hold anything only the site
understands. The values come from the issue (orzelig/ngp-sync#15), copied from
the phone's `Ui.java`, which the crew cannot read.

## Decision

1. **`site/tokens.css` is tokens only.** One `:root { … }` rule of custom
   properties, prefixed `--ngp-`: no other selector, no `@import`, no `url()`,
   no `@media`. That is what makes the file shareable with the browser and the
   phone later. The names are:
   - colours: `--ngp-bg`, `--ngp-text` to `--ngp-text-4`, `--ngp-hairline`,
     `--ngp-glass-1` to `-3`, `--ngp-urgent`, `--ngp-urgent-soft`, `--ngp-ok`,
     `--ngp-accent`;
   - brand: `--ngp-brand-hi`, `-lo`, `-ring`, `-core`;
   - type: `--ngp-size-40`, `-28`, `-20`, `-17`, `-15`, `-14`, `-12`, `-11`, and
     `--ngp-font`.

   Sizes are named by their pixel value because the phone's role names were
   not readable; the issue that shares the file can rename them in one place.
2. **Pages keep their own `<style>` blocks.** Each links
   `<link rel="stylesheet" href="/tokens.css">` before its `<style>` and uses
   only `var(--ngp-…)` for colour, font size and font family. The one literal
   is the `theme-color` meta, which cannot read a custom property and must
   equal `--ngp-bg`.
3. **No animation.** The orb is the working indicator, and nothing on these
   pages waits visibly (the manifest fetch is silent and has a fallback). So the
   site has no orb, no transition, no animation and no `@keyframes`. The brand
   mark is static.
4. **One accent.** Links and the primary download button use `--ngp-accent`,
   with `--ngp-bg` as the button's label colour. Orange appears only inside the
   brand mark.
5. **Always dark.** No `prefers-color-scheme`. Each page sets
   `color-scheme: dark` in its own `<style>`. It is not a token, so it is not in
   `tokens.css`.

`site/index.test.js` enforces all of this for every `site/*.html`, and checks
the WCAG contrast of the text tokens against the background and each glass
layer.

## What this rules out

- **A shared `site/site.css` for layout and components.** It would remove the
  CSS duplicated between the legal pages, but it is a second file the request
  did not ask for and a larger change to review by eye. A later change can add
  it without touching `tokens.css`.
- **Component styles inside `tokens.css`.** The file could then not be shared
  with the phone or the browser, which is the reason it exists.
- **Animating the brand mark as the orb.** An orb that moves while nothing is
  working makes it mean nothing where it is a working indicator.
- **A light theme, or a second accent.** design3 is always dark with one
  accent.
- **A separate `site/tokens.test.js`.** It needs a `Makefile` edit, a Human
  review path, for no gain in coverage.

## What would make this worth revisiting

- The browser or the phone consumes the file and needs role names (or a format
  other than CSS custom properties).
- The site gains something that visibly waits, which is where the orb would
  belong.
- design3 gains a light variant, or the web gets a wallpaper-derived accent.
