/**
 * WCAG AA for the design tokens, in both themes: every text colour against
 * the surfaces it is used on, and the filled buttons. Translucent surfaces are
 * composited the way they are painted: washes on a glass panel, glass on the
 * wall colour (the wallpaper is light at dawn and absent at dusk).
 */
import { readdirSync, readFileSync } from 'node:fs';
import { join, relative } from 'node:path';
import { describe, expect, it } from 'vitest';

type RGBA = [r: number, g: number, b: number, a: number];
type Theme = Record<string, string>;

const css = readFileSync(join(__dirname, 'tokens.css'), 'utf8').replace(/\/\*[\s\S]*?\*\//g, '');

function block(selector: string): Theme {
  const start = css.indexOf(`${selector} {`);
  if (start < 0) throw new Error(`no ${selector} block in tokens.css`);
  const body = css.slice(start, css.indexOf('}', start));
  return Object.fromEntries([...body.matchAll(/(--[\w-]+)\s*:\s*([^;]+);/g)].map((m) => [m[1]!, m[2]!.trim()]));
}

const dawn = block(':root');
const THEMES: Record<string, Theme> = { dawn, dusk: { ...dawn, ...block(":root[data-theme='dusk']") } };

function resolve(theme: Theme, value: string, depth = 0): string {
  if (depth > 10) throw new Error(`var() loop at ${value}`);
  const m = /^var\((--[\w-]+)\)$/.exec(value);
  return m ? resolve(theme, theme[m[1]!] ?? '', depth + 1) : value;
}

function color(theme: Theme, name: string): RGBA {
  const v = resolve(theme, theme[name] ?? '');
  const hex = /^#([0-9a-f]{6})$/i.exec(v);
  if (hex) return [0, 2, 4].map((i) => parseInt(hex[1]!.slice(i, i + 2), 16)).concat(1) as RGBA;
  const rgba = /^rgba?\(\s*([\d.]+)\s*,\s*([\d.]+)\s*,\s*([\d.]+)\s*(?:,\s*([\d.]+)\s*)?\)$/.exec(v);
  if (rgba) return [Number(rgba[1]), Number(rgba[2]), Number(rgba[3]), rgba[4] === undefined ? 1 : Number(rgba[4])];
  throw new Error(`${name}: "${v}" is not a colour`);
}

/** `top` painted over an opaque `bottom`. */
function over(top: RGBA, bottom: RGBA): RGBA {
  const a = top[3];
  return [0, 1, 2].map((i) => top[i]! * a + bottom[i]! * (1 - a)).concat(1) as RGBA;
}

function luminance([r, g, b]: RGBA): number {
  const lin = (c: number) => {
    const s = c / 255;
    return s <= 0.03928 ? s / 12.92 : ((s + 0.055) / 1.055) ** 2.4;
  };
  return 0.2126 * lin(r) + 0.7152 * lin(g) + 0.0722 * lin(b);
}

function contrast(a: RGBA, b: RGBA): number {
  const [hi, lo] = [luminance(a), luminance(b)].sort((x, y) => y - x) as [number, number];
  return (hi + 0.05) / (lo + 0.05);
}

/** A surface as painted: translucent tokens over glass, glass over the wall. */
function surface(theme: Theme, name: string): RGBA {
  const wall = color(theme, '--wall-color');
  const glass = over(color(theme, '--glass-bg'), wall);
  if (name === '--wall-color') return wall;
  if (name === '--glass-bg') return glass;
  return over(color(theme, name), glass);
}

const TEXT = ['--ink-strong', '--ink', '--ink-2', '--ink-3'];
const SURFACES = [
  '--wall-color',
  '--glass-bg',
  '--glass-bg-strong',
  '--glass-fallback',
  '--surface-solid',
  '--surface-muted',
  '--surface-soft',
  '--menu-hover',
  '--wash-faint',
  '--wash-hover',
  '--wash',
  '--wash-strong',
  '--field-bg',
  '--popover-bg',
];

/** Text on a filled, tinted or code background, as the components pair them. */
const PAIRS: [fg: string, bg: string][] = [
  ['--ink-inverse', '--lake'],
  ['--ink-inverse', '--lake-hover'],
  ['--ink-inverse', '--bad'],
  ['--ink-inverse', '--bad-strong'],
  ['--button-dark-ink', '--button-dark-bg'],
  ['--button-dark-ink', '--button-dark-hover'],
  ['--lake-ink', '--lake-soft'],
  ['--lake-ink', '--surface-solid'],
  ['--lake-ink', '--glass-bg'],
  ['--lake-ink', '--wash'],
  ['--lake-ink-hover', '--surface-solid'],
  ['--ok-ink', '--ok-bg'],
  ['--warn-ink', '--warn-bg'],
  ['--bad-ink', '--bad-bg'],
  ['--bad-ink', '--surface-solid'],
  ['--bad-ink', '--glass-bg'],
  ['--bad-ink', '--bad-hover'],
  ['--ok-ink', '--glass-bg'],
  ['--peach-ink', '--peach-bg'],
  ['--violet-ink', '--violet-bg'],
  ['--sage-ink', '--sage-bg'],
  ['--org-ink', '--org-bg'],
  ['--code-ink', '--code-bg'],
  ['--code-comment', '--code-bg'],
  ['--code-warn', '--code-bg'],
  ['--code-error', '--code-bg'],
];

/** Text on a code block's button: the button's tint over the code background. */
const onCode = (theme: Theme, tint: string) => over(color(theme, tint), color(theme, '--code-bg'));

describe.each(Object.keys(THEMES))('%s tokens meet WCAG AA', (name) => {
  const theme = THEMES[name]!;

  it.each(TEXT.flatMap((fg) => SURFACES.map((bg) => [fg, bg] as const)))('%s on %s', (fg, bg) => {
    expect(contrast(color(theme, fg), surface(theme, bg))).toBeGreaterThanOrEqual(4.5);
  });

  it.each(PAIRS)('%s on %s', (fg, bg) => {
    const back = color(theme, bg)[3] < 1 ? surface(theme, bg) : color(theme, bg);
    expect(contrast(color(theme, fg), back)).toBeGreaterThanOrEqual(4.5);
  });

  it('code block buttons', () => {
    for (const tint of ['--code-control', '--code-control-hover']) {
      expect(contrast(color(theme, '--code-ink'), onCode(theme, tint))).toBeGreaterThanOrEqual(4.5);
    }
  });

  it('the filled primary button and the focus mark stand out from the page (3:1, WCAG 1.4.11)', () => {
    expect(contrast(color(theme, '--lake'), surface(theme, '--glass-bg'))).toBeGreaterThanOrEqual(3);
    expect(contrast(color(theme, '--button-dark-bg'), surface(theme, '--glass-bg'))).toBeGreaterThanOrEqual(3);
  });
});

describe('contrast', () => {
  it('matches the WCAG reference values', () => {
    expect(contrast([0, 0, 0, 1], [255, 255, 255, 1])).toBeCloseTo(21, 5);
    expect(contrast([119, 119, 119, 1], [255, 255, 255, 1])).toBeCloseTo(4.48, 2);
  });
});

/**
 * Surface colours written straight into a stylesheet do not follow the theme:
 * a white wash that lifts a row at dawn glares at dusk. They belong in
 * tokens.css. The stylesheets listed here still carry some, to be converted
 * with those pages; the counts only go down.
 */
const NOT_YET_TOKENS: Record<string, number> = {
  'styles/ops.css': 3, // audit table rows, the System page's endpoint value
  'pages/settings/hub.css': 1,
  'pages/settings/settings.css': 6,
  'pages/system/system.css': 6,
};

function styleSheets(dir: string): string[] {
  return readdirSync(dir, { withFileTypes: true }).flatMap((e) => {
    const p = join(dir, e.name);
    if (e.isDirectory()) return styleSheets(p);
    return e.name.endsWith('.css') ? [p] : [];
  });
}

describe('stylesheets', () => {
  const src = join(__dirname, '..');
  it.each(styleSheets(src).map((p) => relative(src, p)).filter((p) => p !== 'styles/tokens.css'))('%s takes its surface colours from tokens', (file) => {
    const body = readFileSync(join(src, file), 'utf8').replace(/\/\*[\s\S]*?\*\//g, '');
    // Colours outside shadows and masks, where a hardcoded tint is the same in both themes.
    const literal = body
      .split(/[;{}]/)
      .filter((decl) => !/^\s*(box-shadow|mask-image|-webkit-mask-image)\s*:/.test(decl))
      .filter((decl) => /#[0-9a-f]{3,8}\b|rgba?\(/i.test(decl));
    expect(literal.length).toBeLessThanOrEqual(NOT_YET_TOKENS[file] ?? 0);
  });
});
