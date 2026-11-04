# Hangar · Dawn — design system

Calm, easy on the eyes: a misty lake-at-dawn wallpaper behind frosted-glass panels
with soft shadows.
Wallpaper file: `web/public/lake-dawn.jpg` (2400×1500).

## Tokens (put in `web/src/styles/tokens.css` as CSS variables)

```css
:root {
  /* ground */
  --wall-color: #DCE3EC;              /* shown while wallpaper loads */
  --wall-image: url(/lake-dawn.jpg);

  /* glass surfaces */
  --glass-bg: rgba(255,255,255,0.66);
  --glass-bg-strong: rgba(255,255,255,0.85);  /* inputs, secondary buttons */
  --glass-border: rgba(255,255,255,0.80);
  --glass-blur: blur(22px) saturate(140%);
  --shadow-panel: 0 1px 2px rgba(40,60,90,.06), 0 16px 40px -14px rgba(40,60,90,.18);
  --shadow-raised: 0 1px 2px rgba(40,60,90,.08), 0 4px 12px -4px rgba(40,60,90,.12);
  --shadow-primary: 0 10px 20px -10px rgba(47,106,138,.7);
  --hairline: rgba(30,43,58,0.07);
  --track: rgba(30,43,58,0.08);

  /* text */
  --ink-strong: #172433;  /* page titles */
  --ink: #1E2B3A;         /* body */
  --ink-2: #3E4C5C;       /* secondary */
  --ink-3: #5A6878;       /* labels, meta — min 4.5:1 on glass */

  /* brand */
  --lake: #2F6A8A;        /* primary buttons, active marks */
  --lake-ink: #1F5673;    /* links, active nav text */
  --lake-soft: #DCEAF3;   /* info pills */
  --chart-1: #8DB4CC;     /* downloads */
  --chart-2: #D98E63;     /* uploads, warning bars (>75%) */
  --chart-3: #5C93B4;     /* meters */

  /* status: always paired with a text label */
  --ok: #3C9A72;   --ok-bg: #DDF0E6;   --ok-ink: #1E6B4A;
  --warn: #D2A23A; --warn-bg: #F7E9CF; --warn-ink: #7A5410;
  --bad: #B23B3B;  --bad-bg: #F6DCDC;  --bad-ink: #8E2A2A;
  --peach-bg: #F7E1D3; --peach-ink: #8A4220;   /* proxy, npm */
  --violet-bg: #E8E2F2; --violet-ink: #4E3A7A; /* group, maven */
  --sage-bg: #E3EEDC; --sage-ink: #3B5E2B;     /* pypi */

  /* type */
  --font-sans: 'Manrope', system-ui, sans-serif;
  --font-mono: 'JetBrains Mono', ui-monospace, monospace;

  /* radius & space */
  --r-panel: 18px; --r-card: 20px; --r-control: 12px; --r-small: 10px; --r-pill: 999px;
  --gap: 16px; --page-pad: 16px;
}
```

## Type scale
Page title 32/700 (−0.02em) · overview hero 24/700 · section title 16/700 · body 14–15/500 ·
labels 12–13/600 · KPI numbers 28–32/700 · code 12–13 mono. Self-host Manrope and
JetBrains Mono (`@fontsource`), no external font requests in production.

## Layout
- Page = wallpaper + `--page-pad` padding; a floating glass **sidebar** (240 px) and a
  content column; both wrap to one column under ~860 px.
- Content: glass top bar (breadcrumb, ⌘K search, bell, avatar) → page header with
  title, one-sentence explanation, primary action → glass panels in grids with 16 px gaps.
  The Overview drops the bar: its header is the breadcrumb, a system state pill, bell and
  avatar, and its hero carries the search box.
- The sidebar's links scroll on short screens; the system status card (state, nodes up,
  version) stays pinned at its bottom and links to System status.
- Tables live inside a panel; wide tables scroll horizontally inside it.

## Components
- **Panel**: `background: var(--glass-bg); backdrop-filter: var(--glass-blur); border: 1px solid var(--glass-border); box-shadow: var(--shadow-panel); border-radius: var(--r-panel)`.
- **Primary button**: lake bg, white text, `--shadow-primary`, 44–48 px tall.
- **Secondary button**: `--glass-bg-strong`, 1px hairline border, ink text.
- **Segmented control**: glass track; active segment white + `--shadow-raised` + lake-ink text.
- **Nav item**: icon (18 px, 1.7 stroke, `currentColor`) + label; active = white bg, raised shadow, lake-ink, 700.
- **Pill**: 12/600, `--r-pill`, tinted bg + matching ink (status, type, severity).
- **Format badge**: 32–44 px rounded square, tinted, 10–11 px uppercase code (OCI, NPM, MVN, PY, HELM).
- **Meter**: 6–10 px track, rounded fill; turns `--chart-2` above 75%.
- **Account menu**: opaque `--surface-solid` card (300 px, radius 16, `--shadow-menu`), header with
  avatar, name, email and role pill; items 44 px with a 30 px icon tile (`--menu-tile`, lake-soft
  when hovered or focused, `--menu-hover` row); Sign out in the danger tone. Menu-button keyboard
  pattern: arrows, Home/End, Escape returns focus to the avatar.
- **Code block**: `#1F2B38` bg, `#DCE6EF` text, comments `#8FA3B5`, copy button.

## Rules
- Never put colour alone on meaning — every status dot has a word next to it.
- Touch targets ≥ 44 px; every control is a real `<button>`, `<a>`, `<input>`.
- Icon-only buttons have `aria-label`.
- Respect `prefers-reduced-motion`; transitions ≤ 150 ms, opacity/transform only.
- Fallback when `backdrop-filter` is unsupported: `--glass-bg` at 0.9 opacity.
- Dark mode (later milestone): "dusk" wallpaper variant, same structure.
