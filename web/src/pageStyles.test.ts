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

describe('page styles', () => {
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
