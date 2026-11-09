/**
 * A small CommonMark-flavoured Markdown renderer for package READMEs that
 * builds React elements only: no HTML string is ever injected, so markup in
 * a README cannot run. Raw HTML is reduced to its text (a few common tags
 * are translated first: <img>, <a>, <br>), links keep only http(s), mailto
 * and in-page targets. Images load only from the URLs the server resolved
 * for them (same-origin by default), lazily and without a referrer; any
 * other image, or one that fails to load, shows its alternative text.
 */
import { Fragment, useLayoutEffect, useRef, useState, type ReactNode } from 'react';
import { safeHref, safeImageSrc } from './urlPolicy';

/* ---------------------------------------------------------------- raw HTML */

/** Decodes the character references of attribute values (the server decodes the same set). */
function decodeEntities(s: string): string {
  return s.replace(/&(#[0-9]{1,7}|#[xX][0-9a-fA-F]{1,6}|amp|lt|gt|quot|apos);/g, (e, name: string) => {
    const named: Record<string, string> = { amp: '&', lt: '<', gt: '>', quot: '"', apos: "'" };
    if (named[name]) return named[name]!;
    const n = name[1] === 'x' || name[1] === 'X' ? parseInt(name.slice(2), 16) : parseInt(name.slice(1), 10);
    return n > 0 && n <= 0x10ffff && (n < 0xd800 || n > 0xdfff) ? String.fromCodePoint(n) : e;
  });
}

const attr = (tag: string, name: string) =>
  decodeEntities(new RegExp(`\\s${name}\\s*=\\s*(?:"([^"]*)"|'([^']*)'|([^\\s>]+))`, 'i').exec(tag)?.slice(1).find((v) => v !== undefined) ?? '');

/** A pixel size from an HTML width/height attribute ("200", "200px"); percentages are ignored. */
const pixels = (v: string) => (/^\s*\d{1,4}(\.\d+)?\s*(px)?\s*$/i.test(v) ? String(Math.round(parseFloat(v))) : '');

/** Reduces inline HTML to Markdown and text. */
function stripHtml(line: string): string {
  return line
    .replace(/<img\b[^>]*>/gi, (tag) => {
      const src = attr(tag, 'src').trim().split(/\s+/)[0];
      if (!src) return '';
      // The size travels after the destination, where Markdown keeps a title.
      const w = pixels(attr(tag, 'width'));
      const h = pixels(attr(tag, 'height'));
      return `![${attr(tag, 'alt').replace(/[[\]]/g, '')}](${src}${w || h ? ` =${w}x${h}` : ''})`;
    })
    .replace(/<a\b[^>]*>([\s\S]*?)<\/a>/gi, (whole, text: string) => {
      const href = attr(whole, 'href');
      return href ? `[${text}](${href})` : text;
    })
    .replace(/<br\s*\/?>/gi, '  \n')
    .replace(/<\/?[a-z][a-z0-9-]*(?:\s[^<>]*)?\/?>/gi, '');
}

function preprocess(src: string): string[] {
  const text = src
    .replace(/\r\n?/g, '\n')
    .replace(/<!--[\s\S]*?-->/g, '')
    .replace(/<(script|style|iframe|object|embed|template)\b[\s\S]*?<\/\1\s*>/gi, '');
  const out: string[] = [];
  let fence: string | null = null;
  for (const line of text.split('\n')) {
    const m = /^ {0,3}(`{3,}|~{3,})/.exec(line);
    if (fence) {
      out.push(line);
      if (m && m[1]![0] === fence[0] && m[1]!.length >= fence.length && line.trim() === m[1]) fence = null;
      continue;
    }
    if (m) {
      fence = m[1]!;
      out.push(line);
      continue;
    }
    out.push(...stripHtml(line).split('\n'));
  }
  return out;
}

/* ---------------------------------------------------------------- blocks */

type Refs = Map<string, { href: string; title?: string }>;

/** What inline rendering needs besides the text: link reference definitions and image sources. */
interface Ctx {
  refs: Refs;
  /** Image source as written -> URL to load (from the package info API). */
  images: Readonly<Record<string, string>>;
  /** No image is loaded at all (advisories): each shows its alternative text. */
  noImages: boolean;
  /** Added to a Markdown heading level ("#" is 1) to get the HTML heading level. */
  headingShift: number;
  /** Links are shown as their text (inside buttons and other links). */
  noLinks?: boolean;
}

const BLANK = /^\s*$/;
const HEADING = /^ {0,3}(#{1,6})(?:\s+(.*?))?\s*#*\s*$/;
const HR = /^ {0,3}([-*_])(?:\s*\1){2,}\s*$/;
const FENCE = /^ {0,3}(`{3,}|~{3,})\s*([^`\s]*)/;
const BULLET = /^( {0,3})([-*+])(\s+|$)(.*)$/;
const ORDERED = /^( {0,3})(\d{1,9})([.)])(\s+|$)(.*)$/;
const QUOTE = /^ {0,3}>\s?(.*)$/;
const TABLE_SEP = /^\s*\|?\s*:?-{1,}:?\s*(\|\s*:?-{1,}:?\s*)*\|?\s*$/;
const REF_DEF = /^ {0,3}\[([^\]]+)\]:\s*<?(\S+?)>?(?:\s+["'(](.*)["')])?\s*$/;

const isBlockStart = (l: string) => HEADING.test(l) || HR.test(l) || FENCE.test(l) || QUOTE.test(l) || BULLET.test(l) || ORDERED.test(l);

const splitRow = (row: string) =>
  row
    .trim()
    .replace(/^\|/, '')
    .replace(/(?<!\\)\|$/, '')
    .split(/(?<!\\)\|/)
    .map((c) => c.trim().replace(/\\\|/g, '|'));

function blocks(lines: string[], ctx: Ctx, key: string): ReactNode[] {
  const out: ReactNode[] = [];
  let i = 0;
  const k = () => `${key}.${out.length}`;
  while (i < lines.length) {
    const line = lines[i]!;
    if (BLANK.test(line)) {
      i++;
      continue;
    }
    let m: RegExpExecArray | null;
    if ((m = FENCE.exec(line))) {
      const marker = m[1]!;
      const lang = m[2] ?? '';
      const body: string[] = [];
      i++;
      while (i < lines.length && !(lines[i]!.trim().startsWith(marker[0]!.repeat(marker.length)) && lines[i]!.trim().replace(new RegExp(`^\\${marker[0]}+`), '') === '')) {
        body.push(lines[i]!);
        i++;
      }
      i++;
      out.push(
        <CodePre key={k()}>
          <code data-lang={lang || undefined}>{body.join('\n')}</code>
        </CodePre>,
      );
      continue;
    }
    if (/^( {4}|\t)/.test(line)) {
      const body: string[] = [];
      while (i < lines.length && (/^( {4}|\t)/.test(lines[i]!) || (BLANK.test(lines[i]!) && /^( {4}|\t)/.test(lines[i + 1] ?? '')))) {
        body.push(lines[i]!.replace(/^( {4}|\t)/, ''));
        i++;
      }
      out.push(
        <CodePre key={k()}>
          <code>{body.join('\n')}</code>
        </CodePre>,
      );
      continue;
    }
    if ((m = HEADING.exec(line))) {
      out.push(heading(m[1]!.length, ctx.headingShift, inline(m[2] ?? '', ctx, k()), k()));
      i++;
      continue;
    }
    if (HR.test(line)) {
      out.push(<hr key={k()} />);
      i++;
      continue;
    }
    if (QUOTE.test(line)) {
      const inner: string[] = [];
      while (i < lines.length && !BLANK.test(lines[i]!)) {
        const q = QUOTE.exec(lines[i]!);
        inner.push(q ? q[1]! : lines[i]!);
        i++;
      }
      out.push(<blockquote key={k()}>{blocks(inner, ctx, k())}</blockquote>);
      continue;
    }
    if (BULLET.test(line) || ORDERED.test(line)) {
      const ordered = !BULLET.test(line);
      const items: string[][] = [];
      let start = 1;
      while (i < lines.length) {
        const l = lines[i]!;
        const b = ordered ? ORDERED.exec(l) : BULLET.exec(l);
        if (b) {
          if (ordered && items.length === 0) start = Number(b[2]);
          const markerWidth = ordered ? b[1]!.length + b[2]!.length + 1 + (b[4]!.length || 1) : b[1]!.length + 1 + (b[3]!.length || 1);
          items.push([ordered ? b[5]! : b[4]!]);
          i++;
          // Continuation and nested lines: indented, or lazy paragraph text.
          while (i < lines.length) {
            const c = lines[i]!;
            if (BLANK.test(c)) {
              const nextLine = lines[i + 1] ?? '';
              if (/^\s{2,}\S/.test(nextLine)) {
                items[items.length - 1]!.push('');
                i++;
                continue;
              }
              break;
            }
            const indent = /^\s*/.exec(c)![0].length;
            if (indent >= Math.min(markerWidth, 4) && indent >= 2) {
              items[items.length - 1]!.push(c.slice(Math.min(indent, markerWidth)));
              i++;
              continue;
            }
            if (!isBlockStart(c) && indent < 2) {
              items[items.length - 1]!.push(c.trimStart());
              i++;
              continue;
            }
            break;
          }
          continue;
        }
        if (BLANK.test(l) && (ordered ? ORDERED : BULLET).test(lines[i + 1] ?? '')) {
          i++;
          continue;
        }
        break;
      }
      const lis = items.map((it, n) => {
        const tight = !it.some((x) => BLANK.test(x));
        const content = blocks(it, ctx, `${k()}.${n}`);
        const task = /^\[([ xX])\]\s+/.exec(it[0] ?? '');
        return (
          <li key={n} className={task ? 'md-task' : undefined}>
            {tight && content.length === 1 && isParagraph(content[0]) ? (content[0] as { props: { children: ReactNode } }).props.children : content}
          </li>
        );
      });
      out.push(
        ordered ? (
          <ol key={k()} start={start !== 1 ? start : undefined}>
            {lis}
          </ol>
        ) : (
          <ul key={k()}>{lis}</ul>
        ),
      );
      continue;
    }
    if (line.includes('|') && TABLE_SEP.test(lines[i + 1] ?? '') && (lines[i + 1] ?? '').includes('-')) {
      const head = splitRow(line);
      const aligns = splitRow(lines[i + 1]!).map((c) => (c.startsWith(':') && c.endsWith(':') ? 'center' : c.endsWith(':') ? 'right' : undefined));
      i += 2;
      const body: string[][] = [];
      while (i < lines.length && !BLANK.test(lines[i]!) && lines[i]!.includes('|')) {
        body.push(splitRow(lines[i]!));
        i++;
      }
      const key = k();
      out.push(
        <div key={key} className="md-table">
          <table>
            <thead>
              <tr>
                {head.map((c, n) => (
                  <th key={n} style={{ textAlign: aligns[n] }}>
                    {inline(c, ctx, `${key}.h${n}`)}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              {body.map((r, rn) => (
                <tr key={rn}>
                  {head.map((_, n) => (
                    <td key={n} style={{ textAlign: aligns[n] }}>
                      {inline(r[n] ?? '', ctx, `${key}.${rn}.${n}`)}
                    </td>
                  ))}
                </tr>
              ))}
            </tbody>
          </table>
        </div>,
      );
      continue;
    }
    // Paragraph, possibly a setext heading.
    const para: string[] = [];
    while (i < lines.length && !BLANK.test(lines[i]!) && (para.length === 0 || !isBlockStart(lines[i]!) || /^ {0,3}(=+|-+)\s*$/.test(lines[i]!))) {
      const setext = /^ {0,3}(=+|-+)\s*$/.exec(lines[i]!);
      if (setext && para.length > 0) {
        out.push(heading(setext[1]![0] === '=' ? 1 : 2, ctx.headingShift, inline(para.join('\n'), ctx, k()), k()));
        para.length = 0;
        i++;
        break;
      }
      para.push(lines[i]!);
      i++;
    }
    if (para.length) out.push(<p key={k()}>{inline(para.join('\n'), ctx, k())}</p>);
  }
  return out;
}

const isParagraph = (n: ReactNode) => typeof n === 'object' && n !== null && 'type' in n && n.type === 'p';

function heading(level: number, shift: number, children: ReactNode, key: string): ReactNode {
  // Markdown headings sit below the page's own headings.
  const shifted = Math.min(6, level + shift);
  const Tag = `h${shifted}` as 'h3';
  return (
    <Tag key={key} className={`md-h${level}`}>
      {children}
    </Tag>
  );
}

/** A code block; one that scrolls is focusable, so keyboards can scroll it. */
function CodePre({ children }: { children: ReactNode }) {
  const ref = useRef<HTMLPreElement>(null);
  const [scrolls, setScrolls] = useState(false);
  useLayoutEffect(() => {
    const el = ref.current;
    if (el) setScrolls(el.scrollHeight > el.clientHeight + 1 || el.scrollWidth > el.clientWidth + 1);
  }, [children]);
  return (
    <pre ref={ref} className="md-code" tabIndex={scrolls ? 0 : undefined}>
      {children}
    </pre>
  );
}

/* ---------------------------------------------------------------- inlines */

/** Index of the bracket closing the one at `open`, or -1. */
function closing(s: string, open: number, o: string, c: string): number {
  let depth = 0;
  for (let i = open; i < s.length; i++) {
    if (s[i] === '\\') {
      i++;
      continue;
    }
    if (s[i] === o) depth++;
    else if (s[i] === c && --depth === 0) return i;
  }
  return -1;
}

interface LinkParse {
  text: string;
  href: string;
  end: number;
  /** What follows the destination inside the parentheses (a title, or an image size "=WxH"). */
  rest?: string;
}

/** Parses [text](href "title"), [text][ref] or [text] starting at the "[" at `at`. */
function parseLink(s: string, at: number, ctx: Ctx): LinkParse | null {
  const close = closing(s, at, '[', ']');
  if (close < 0) return null;
  const text = s.slice(at + 1, close);
  if (s[close + 1] === '(') {
    const end = closing(s, close + 1, '(', ')');
    if (end < 0) return null;
    const dest = s.slice(close + 2, end).trim();
    const angle = /^<([^>]*)>/.exec(dest);
    const href = angle?.[1] ?? dest.split(/\s+/)[0] ?? '';
    return { text, href, end: end + 1, rest: dest.slice(angle ? angle[0].length : href.length).trim() };
  }
  if (s[close + 1] === '[') {
    const end = s.indexOf(']', close + 2);
    if (end < 0) return null;
    const ref = ctx.refs.get((s.slice(close + 2, end) || text).toLowerCase());
    return ref ? { text, href: ref.href, end: end + 1 } : null;
  }
  const ref = ctx.refs.get(text.toLowerCase());
  return ref ? { text, href: ref.href, end: close + 1 } : null;
}

const PUNCT = /[!"#$%&'()*+,\-./:;<=>?@[\\\]^_`{|}~]/;
const BARE_URL = /^https?:\/\/[^\s<>()]+[^\s<>().,;:!?'"*_~]/;

function inline(src: string, ctx: Ctx, key: string): ReactNode[] {
  const out: ReactNode[] = [];
  let text = '';
  const flush = () => {
    if (text) out.push(text);
    text = '';
  };
  const push = (n: ReactNode) => {
    flush();
    out.push(<Fragment key={`${key}.${out.length}`}>{n}</Fragment>);
  };
  let i = 0;
  while (i < src.length) {
    const ch = src[i]!;
    const rest = src.slice(i);
    if (ch === '\\' && src[i + 1] === '\n') {
      push(<br />);
      i += 2;
      continue;
    }
    if (ch === '\\' && PUNCT.test(src[i + 1] ?? '')) {
      text += src[i + 1];
      i += 2;
      continue;
    }
    if (ch === '\n') {
      if (text.endsWith('  ')) {
        text = text.trimEnd();
        push(<br />);
      } else {
        text = text.trimEnd() + ' ';
      }
      i++;
      continue;
    }
    if (ch === '`') {
      const run = /^`+/.exec(rest)![0];
      const end = src.indexOf(run, i + run.length);
      if (end > 0) {
        push(<code>{src.slice(i + run.length, end).replace(/\n/g, ' ').trim()}</code>);
        i = end + run.length;
        continue;
      }
      text += run;
      i += run.length;
      continue;
    }
    if (ch === '!' && src[i + 1] === '[') {
      const l = parseLink(src, i + 1, ctx);
      if (l) {
        const size = /^=(\d{0,4})x(\d{0,4})$/.exec(l.rest ?? '');
        push(<ReadmeImage src={imageSource(l.href, ctx)} alt={plainText(l.text)} width={size?.[1]} height={size?.[2]} />);
        i = l.end;
        continue;
      }
    }
    if (ch === '[') {
      const l = parseLink(src, i, ctx);
      if (l) {
        const href = ctx.noLinks ? null : safeHref(l.href);
        const children = inline(l.text, ctx, `${key}.${out.length}l`);
        push(
          href ? (
            <a href={href} {...(href.startsWith('#') ? {} : { target: '_blank', rel: 'noopener noreferrer nofollow' })}>
              {children}
            </a>
          ) : ctx.noLinks ? (
            <span>{children}</span>
          ) : (
            <span className="md-link-blocked" title="Relative link: not available here">
              {children}
            </span>
          ),
        );
        i = l.end;
        continue;
      }
    }
    if (ch === '<') {
      const m = /^<((?:https?|mailto):[^\s<>]+)>/i.exec(rest);
      if (m) {
        const href = ctx.noLinks ? null : safeHref(m[1]!);
        push(href ? <a href={href} target="_blank" rel="noopener noreferrer nofollow">{m[1]}</a> : m[1]);
        i += m[0].length;
        continue;
      }
    }
    if ((ch === 'h' || ch === 'H') && !/[\w/]$/.test(text)) {
      const m = BARE_URL.exec(rest);
      if (m) {
        const href = ctx.noLinks ? null : safeHref(m[0]);
        push(href ? <a href={href} target="_blank" rel="noopener noreferrer nofollow">{m[0]}</a> : m[0]);
        i += m[0].length;
        continue;
      }
    }
    if (ch === '~' && src[i + 1] === '~') {
      const end = src.indexOf('~~', i + 2);
      if (end > i + 2) {
        push(<del>{inline(src.slice(i + 2, end), ctx, `${key}.${out.length}d`)}</del>);
        i = end + 2;
        continue;
      }
    }
    if (ch === '*' || ch === '_') {
      const leftFlank = ch === '*' || !/[\p{L}\p{N}]/u.test(src[i - 1] ?? '');
      const dbl = src[i + 1] === ch;
      const marker = dbl ? ch + ch : ch;
      if (leftFlank && src[i + marker.length] && !/\s/.test(src[i + marker.length]!)) {
        let end = src.indexOf(marker, i + marker.length + 1);
        while (end > 0 && (/\s/.test(src[end - 1]!) || (!dbl && src[end + 1] === ch) || (ch === '_' && /[\p{L}\p{N}]/u.test(src[end + marker.length] ?? '')))) {
          end = src.indexOf(marker, end + 1);
        }
        if (end > 0) {
          const inner = inline(src.slice(i + marker.length, end), ctx, `${key}.${out.length}e`);
          push(dbl ? <strong>{inner}</strong> : <em>{inner}</em>);
          i = end + marker.length;
          continue;
        }
      }
    }
    text += ch;
    i++;
  }
  flush();
  return out;
}

/* ---------------------------------------------------------------- images */

/**
 * The URL to load for an image source: the one the server resolved for it,
 * or an inline data image. Anything else is never loaded from the browser.
 */
function imageSource(href: string, ctx: Ctx): string | null {
  if (ctx.noImages) return null;
  const h = href.trim();
  if (Object.hasOwn(ctx.images, h)) return safeImageSrc(ctx.images[h]!);
  return h.startsWith('data:') ? safeImageSrc(h) : null;
}

/** Alternative text without Markdown punctuation ("**Build**" -> "Build"). */
const plainText = (s: string) =>
  s
    .replace(/\\([!-/:-@[-`{-~])/g, '$1')
    .replace(/[*_`~]/g, '')
    .trim();

/**
 * A README image: bounded in size, loaded lazily without a referrer. An
 * image the server did not resolve, or one that fails to load, becomes a
 * small chip with its alternative text.
 */
function ReadmeImage({ src, alt, width, height }: { src: string | null; alt: string; width?: string; height?: string }) {
  const [failed, setFailed] = useState(false);
  if (!src || failed) return <ImageChip alt={alt} failed={failed} />;
  return (
    <img
      src={src}
      alt={alt}
      width={width || undefined}
      height={height || undefined}
      loading="lazy"
      decoding="async"
      referrerPolicy="no-referrer"
      className="md-img"
      onError={() => setFailed(true)}
    />
  );
}

const IconImage = () => (
  <svg width={13} height={13} viewBox="0 0 24 24" fill="none" aria-hidden="true" focusable="false">
    <rect x="3" y="4" width="18" height="16" rx="3" stroke="currentColor" strokeWidth="1.8" />
    <circle cx="9" cy="10" r="1.8" fill="currentColor" />
    <path d="m4 18 5.5-5 4 3.5L17 13l3.5 3.5" stroke="currentColor" strokeWidth="1.8" strokeLinejoin="round" />
  </svg>
);

function ImageChip({ alt, failed }: { alt: string; failed: boolean }) {
  const label = alt || 'Image';
  return (
    <span className="md-img-chip" role="img" aria-label={label} title={failed ? `Image could not be loaded: ${label}` : `Image not shown: ${label}`}>
      <IconImage />
      <span className="md-img-chip__text">{label}</span>
    </span>
  );
}

/* ---------------------------------------------------------------- entry */

interface MarkdownProps {
  source: string;
  /** Image source as written -> URL to load; images without an entry show their alternative text. */
  images?: Readonly<Record<string, string>>;
  /** Load no image at all, not even inline data images. */
  noImages?: boolean;
  /** The HTML level the shallowest heading in `source` gets; by default "#" is an h3. */
  topHeading?: number;
}

/** The shallowest ATX heading level in the source, outside code blocks. */
function shallowestHeading(lines: string[]): number | null {
  let fence = false;
  let min: number | null = null;
  for (const l of lines) {
    if (FENCE.test(l)) fence = !fence;
    const m = !fence ? HEADING.exec(l) : null;
    if (m) min = Math.min(min ?? 6, m[1]!.length);
  }
  return min;
}

/**
 * Renders `source` as Markdown. `images` maps each image source as written
 * in the README to the URL to load (the package info API resolves and signs
 * them); images without an entry show their alternative text.
 */
export function Markdown({ source, images, noImages = false, topHeading }: MarkdownProps) {
  const lines = preprocess(source);
  const top = topHeading !== undefined ? shallowestHeading(lines) : null;
  const headingShift = topHeading !== undefined && top !== null ? topHeading - top : 2;
  const ctx: Ctx = { refs: new Map(), images: images ?? {}, noImages, headingShift };
  const body: string[] = [];
  let inFence = false;
  for (const l of lines) {
    if (FENCE.test(l)) inFence = !inFence;
    const m = !inFence ? REF_DEF.exec(l) : null;
    if (m) ctx.refs.set(m[1]!.toLowerCase(), { href: m[2]!, title: m[3] });
    else body.push(l);
  }
  return <div className="markdown">{blocks(body, ctx, 'md')}</div>;
}

/**
 * One line of Markdown (an advisory title): code spans and emphasis only,
 * links as plain text, so it can sit inside a button or a link.
 */
export function MarkdownInline({ source }: { source: string }) {
  const ctx: Ctx = { refs: new Map(), images: {}, noImages: true, headingShift: 2, noLinks: true };
  return <>{inline(preprocess(source).join(' '), ctx, 'mi')}</>;
}
