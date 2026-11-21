// Renders ../assets/og.png (1200x630), the image link previews show, from an SVG drawn
// here. No browser: resvg rasterises it with Manrope and JetBrains Mono, the web UI's
// fonts, unpacked from @fontsource's WOFF files.
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { basename, dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { inflateSync } from 'node:zlib';
import { Resvg } from '@resvg/resvg-js';

const here = dirname(fileURLToPath(import.meta.url));
const modules = join(here, '..', 'node_modules', '@fontsource');
const out = join(here, '..', '..', 'assets', 'og.png');

// WOFF 1.0 to TrueType: the same tables, each inflated, behind a plain sfnt directory.
// (WOFF rather than WOFF2: @fontsource's WOFF2 Manrope names its family "Manrope
// ExtraLight", which resvg then does not match as "Manrope".)
function woffToSfnt(woff) {
  const numTables = woff.readUInt16BE(12);
  const head = Buffer.alloc(12 + numTables * 16);
  head.writeUInt32BE(woff.readUInt32BE(4), 0);
  head.writeUInt16BE(numTables, 4);
  const parts = [head];
  let pos = head.length;
  for (let i = 0; i < numTables; i++) {
    const e = 44 + i * 20, r = 12 + i * 16;
    const offset = woff.readUInt32BE(e + 4), compLength = woff.readUInt32BE(e + 8), origLength = woff.readUInt32BE(e + 12);
    const data = compLength < origLength ? inflateSync(woff.subarray(offset, offset + compLength)) : woff.subarray(offset, offset + origLength);
    woff.copy(head, r, e, e + 4);
    head.writeUInt32BE(woff.readUInt32BE(e + 16), r + 4);
    head.writeUInt32BE(pos, r + 8);
    head.writeUInt32BE(data.length, r + 12);
    const pad = Buffer.alloc((4 - (data.length % 4)) % 4);
    parts.push(data, pad);
    pos += data.length + pad.length;
  }
  return Buffer.concat(parts);
}

// resvg's native build loads fonts from files.
const fontDir = mkdtempSync(join(tmpdir(), 'hangar-og-'));
const fontFiles = [
  'manrope/files/manrope-latin-500-normal.woff',
  'manrope/files/manrope-latin-700-normal.woff',
  'jetbrains-mono/files/jetbrains-mono-latin-400-normal.woff',
].map((f) => {
  const path = join(fontDir, basename(f).replace(/\.woff$/, '.ttf'));
  writeFileSync(path, woffToSfnt(readFileSync(join(modules, f))));
  return path;
});
const fontOptions = { fontFiles, loadSystemFonts: false, defaultFontFamily: 'Manrope' };
const SANS = 'Manrope';

const FORMATS = ['Docker', 'npm', 'Maven', 'PyPI', 'NuGet', 'Go', 'Helm', 'Cargo', 'APT', 'RPM', 'Raw'];
const CHIP = { size: 20, padX: 15, height: 40, gap: 9 };

// Width of a chip label, measured with the same renderer and font.
function textWidth(text) {
  const svg = `<svg xmlns="http://www.w3.org/2000/svg" width="600" height="60"><text x="0" y="40" font-family="${SANS}" font-weight="700" font-size="${CHIP.size}">${text}</text></svg>`;
  const box = new Resvg(svg, { font: fontOptions }).getBBox();
  return box ? box.width : text.length * CHIP.size * 0.6;
}

let x = 80;
const chips = FORMATS.map((f) => {
  const w = Math.round(textWidth(f)) + CHIP.padX * 2;
  const chip = `<g transform="translate(${x} 458)"><rect width="${w}" height="${CHIP.height}" rx="20" fill="#fff" fill-opacity=".72" stroke="#111" stroke-opacity=".08"/>` +
    `<text x="${w / 2}" y="27" text-anchor="middle" font-family="${SANS}" font-weight="700" font-size="${CHIP.size}" fill="#222">${f}</text></g>`;
  x += w + CHIP.gap;
  return chip;
});

const svg = `<svg xmlns="http://www.w3.org/2000/svg" width="1200" height="630" viewBox="0 0 1200 630">
  <defs>
    <linearGradient id="sky" x1="0" y1="0" x2="0" y2="1">
      <stop offset="0" stop-color="#C9D5E7"/><stop offset=".3" stop-color="#D9DCEA"/><stop offset=".55" stop-color="#EADCE2"/>
      <stop offset=".78" stop-color="#F6E3D8"/><stop offset="1" stop-color="#FBF2EC"/>
    </linearGradient>
    <radialGradient id="glow" cx=".5" cy=".5" r=".5">
      <stop offset="0" stop-color="#F7C4A0" stop-opacity=".8"/><stop offset=".55" stop-color="#F7C4A0" stop-opacity=".25"/><stop offset="1" stop-color="#F7C4A0" stop-opacity="0"/>
    </radialGradient>
  </defs>
  <rect width="1200" height="630" fill="url(#sky)"/>
  <ellipse cx="600" cy="520" rx="650" ry="280" fill="url(#glow)"/>
  <g transform="translate(80 64) scale(1.75)"><rect width="32" height="32" rx="8" fill="#111"/><path d="M8 24V13l8-5 8 5v11" fill="none" stroke="#fff" stroke-width="2.4" stroke-linejoin="round"/></g>
  <text x="154" y="104" font-family="${SANS}" font-weight="700" font-size="34" letter-spacing="-.7" fill="#111">Hangar</text>
  <text x="1120" y="98" text-anchor="end" font-family="JetBrains Mono" font-size="20" fill="#4A4A47">ismoilovdevml.github.io/hangar</text>
  <text x="76" y="290" font-family="${SANS}" font-weight="700" font-size="80" letter-spacing="-3.6" fill="#111">Self-hosted package registry</text>
  <text font-family="${SANS}" font-weight="500" font-size="30" fill="#3E3E3B">
    <tspan x="80" y="356">Cache what you download, store what you publish, see what is</tspan>
    <tspan x="80" y="396">unsafe. On your own servers.</tspan>
  </text>
  ${chips.join('\n  ')}
</svg>`;

const png = new Resvg(svg, { font: fontOptions, fitTo: { mode: 'width', value: 1200 } }).render().asPng();
writeFileSync(out, png);
rmSync(fontDir, { recursive: true, force: true });
console.log(`wrote ${out} (${(png.length / 1024).toFixed(0)} KiB)`);
