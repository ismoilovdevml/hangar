/**
 * Minimal YAML emitter for config-as-code previews: plain objects, arrays,
 * strings, numbers and booleans. Strings are quoted whenever a YAML 1.1/1.2
 * reader could read them as something else; multi-line strings use a literal
 * block. Comments are supported through `YamlComment` values.
 */

export type YamlValue = string | number | boolean | null | YamlValue[] | { [key: string]: YamlValue | undefined } | YamlComment;

/** A `# comment` line in place of a key (its key is ignored). */
export class YamlComment {
  constructor(readonly text: string) {}
}

const AMBIGUOUS = /^(?:~|null|Null|NULL|true|True|TRUE|false|False|FALSE|yes|Yes|YES|no|No|NO|on|On|ON|off|Off|OFF|y|Y|n|N)$/;
/** Whole-string numbers, YAML 1.1 octal/hex/binary/sexagesimal, infinities and timestamps. */
const NUMBERISH =
  /^(?:[-+]?(?:\d[\d_]*(?:\.[\d_]*)?(?:e[-+]?\d+)?|\.\d[\d_]*(?:e[-+]?\d+)?|0x[\da-f_]+|0o?[0-7_]+|0b[01_]+|\.inf|\.nan|\d+(?::[0-5]?\d)+(?:\.\d*)?)|\d{4}-\d\d?-\d\d?.*)$/i;

function needsQuotes(s: string): boolean {
  return (
    s === '' ||
    AMBIGUOUS.test(s) ||
    NUMBERISH.test(s) ||
    /^[\s\-?:,[\]{}#&*!|>'"%@`]/.test(s) ||
    /\s$/.test(s) ||
    /: |:$| #|\t/.test(s)
  );
}

function scalar(s: string): string {
  return needsQuotes(s) ? JSON.stringify(s) : s;
}

function emit(value: YamlValue, indent: number, out: string[], prefix: string): void {
  const pad = ' '.repeat(indent);
  if (value instanceof YamlComment) {
    out.push(`${pad}# ${value.text}`);
    return;
  }
  if (Array.isArray(value)) {
    if (value.length === 0) {
      out.push(`${prefix} []`);
      return;
    }
    out.push(prefix);
    const dash = `${pad}  -`;
    for (const item of value) {
      if (item !== null && typeof item === 'object' && !Array.isArray(item) && !(item instanceof YamlComment)) {
        const lines: string[] = [];
        emitObject(item, indent + 4, lines);
        if (lines.length === 0) out.push(`${dash} {}`);
        else out.push(`${dash} ${lines[0]!.trimStart()}`, ...lines.slice(1));
      } else {
        emit(item, indent + 4, out, dash);
      }
    }
    return;
  }
  if (value !== null && typeof value === 'object') {
    const lines: string[] = [];
    emitObject(value, indent + 2, lines);
    if (lines.length === 0) out.push(`${prefix} {}`);
    else out.push(prefix, ...lines);
    return;
  }
  if (typeof value === 'string' && value.includes('\n')) {
    const chomp = value.endsWith('\n') ? '' : '-';
    out.push(`${prefix} |${chomp}`);
    for (const l of value.replace(/\n$/, '').split('\n')) out.push(l ? `${pad}  ${l}` : '');
    return;
  }
  const text = value === null ? 'null' : typeof value === 'string' ? scalar(value) : String(value);
  out.push(`${prefix} ${text}`);
}

function emitObject(obj: { [key: string]: YamlValue | undefined }, indent: number, out: string[]): void {
  const pad = ' '.repeat(indent);
  for (const [k, v] of Object.entries(obj)) {
    if (v === undefined) continue;
    if (v instanceof YamlComment) {
      out.push(`${pad}# ${v.text}`);
      continue;
    }
    emit(v, indent, out, `${pad}${scalar(k)}:`);
  }
}

export function toYaml(obj: { [key: string]: YamlValue | undefined }): string {
  const out: string[] = [];
  emitObject(obj, 0, out);
  return out.join('\n') + '\n';
}
