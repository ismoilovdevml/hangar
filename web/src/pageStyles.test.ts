import { readdirSync, readFileSync } from 'node:fs';
import { dirname, join, relative, resolve } from 'node:path';

const src = resolve(__dirname);

function sources(dir: string): string[] {
  return readdirSync(dir, { withFileTypes: true }).flatMap((e) => {
    const p = join(dir, e.name);
    if (e.isDirectory()) return e.name === 'test' || e.name === 'mocks' ? [] : sources(p);
    return /\.tsx?$/.test(e.name) && !/\.test\.tsx?$/.test(e.name) ? [p] : [];
  });
}

function styleSheets(dir: string): string[] {
  return readdirSync(dir, { withFileTypes: true }).flatMap((e) => {
    const p = join(dir, e.name);
    if (e.isDirectory()) return styleSheets(p);
    return e.name.endsWith('.css') ? [p] : [];
  });
}

/** Every selector of a sheet with the at-rules around it ("@media (max-width: 640px) .dialog"). */
function selectors(css: string): string[] {
  const out: string[] = [];
  const context: string[] = [];
  let prelude = '';
  for (const ch of css.replace(/\/\*[\s\S]*?\*\//g, '')) {
    if (ch === '{') {
      const head = prelude.trim().replace(/\s+/g, ' ');
      if (head.startsWith('@')) context.push(head);
      else {
        context.push('');
        for (const s of head.split(',')) out.push([...context.slice(0, -1).filter(Boolean), s.trim()].join(' '));
      }
      prelude = '';
    } else if (ch === '}') {
      context.pop();
      prelude = '';
    } else if (ch === ';') {
      prelude = '';
    } else {
      prelude += ch;
    }
  }
  return out;
}

describe('page styles', () => {
  it('defines each selector in one style sheet only', () => {
    // Page sheets load before the global ones (see pageStyles.ts), so a
    // selector defined twice silently takes the later sheet's values on every
    // page: dialogs, chips and fact lists once restyled each other this way.
    const owner = new Map<string, string>();
    const twice: string[] = [];
    for (const file of styleSheets(src)) {
      const name = relative(src, file);
      for (const sel of new Set(selectors(readFileSync(file, 'utf8')))) {
        const first = owner.get(sel);
        if (first && first !== name) twice.push(`${sel}: ${first} and ${name}`);
        else owner.set(sel, name);
      }
    }
    expect(twice).toEqual([]);
  });

  it('lists every style sheet a page or component imports', () => {
    const listed = readFileSync(join(src, 'pageStyles.ts'), 'utf8');
    const missing: string[] = [];
    for (const file of sources(src)) {
      if (file.endsWith('main.tsx') || file.endsWith('pageStyles.ts')) continue;
      for (const m of readFileSync(file, 'utf8').matchAll(/^import '(\.[^']+\.css)';$/gm)) {
        const sheet = './' + relative(src, resolve(dirname(file), m[1] ?? ''));
        // Lazily loaded pages would otherwise get their own style sheet, loaded
        // after the global one, and change how equal rules resolve.
        if (!listed.includes(`import '${sheet}';`)) missing.push(`${relative(src, file)} -> ${sheet}`);
      }
    }
    expect(missing).toEqual([]);
  });
});
