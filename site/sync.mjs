// Copies docs/ from a git ref of the Hangar repository into .cache/docs and
// records which commit it came from.
//
//   DOCS_REF=origin/main (default)  any ref of this repository: main, v0.2.0, a commit
//   DOCS_DIR=/path/to/hangar/docs   a working tree instead of git (no commit, no lastmod)
import { execFileSync } from 'node:child_process';
import { cpSync, mkdirSync, rmSync, writeFileSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));
const cache = join(here, '.cache');
const out = join(cache, 'docs');

rmSync(cache, { recursive: true, force: true });
mkdirSync(cache, { recursive: true });

if (process.env.DOCS_DIR) {
  const src = resolve(process.env.DOCS_DIR);
  cpSync(src, out, { recursive: true });
  writeFileSync(join(cache, 'source.json'), JSON.stringify({ ref: src, commit: null, lastmod: {} }, null, 2));
  console.log(`docs copied from ${src}`);
} else {
  const ref = process.env.DOCS_REF || 'origin/main';
  const top = execFileSync('git', ['rev-parse', '--show-toplevel'], { cwd: here, encoding: 'utf8' }).trim();
  const git = (...args) => execFileSync('git', args, { cwd: top, encoding: 'utf8', maxBuffer: 64 << 20 });
  let commit;
  try {
    commit = git('rev-parse', '--verify', `${ref}^{commit}`).trim();
  } catch {
    console.error(`sync: ${ref} is not a commit here. Run "git fetch origin main" first, or set DOCS_REF or DOCS_DIR.`);
    process.exit(1);
  }
  const tar = execFileSync('git', ['archive', '--format=tar', commit, 'docs'], { cwd: top, maxBuffer: 256 << 20 });
  execFileSync('tar', ['-x', '-C', cache], { input: tar });

  // Last change of every markdown file, for the sitemap.
  const lastmod = {};
  let current = null;
  for (const line of git('log', '--format=@%cI', '--name-only', commit, '--', 'docs').split('\n')) {
    if (line.startsWith('@')) current = line.slice(1);
    else if (line.endsWith('.md') && !(line in lastmod)) lastmod[line] = current;
  }
  const date = git('show', '-s', '--format=%cI', commit).trim();
  writeFileSync(join(cache, 'source.json'), JSON.stringify({ ref, commit, date, lastmod }, null, 2));
  console.log(`docs synced from ${ref} (${commit.slice(0, 7)}, ${date})`);
}
