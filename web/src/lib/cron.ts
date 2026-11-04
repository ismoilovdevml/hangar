/**
 * 5-field cron expressions (minute hour day-of-month month day-of-week), all
 * in UTC as the API defines them: validation, a plain-language description and
 * the next run times. Supports `*`, numbers, ranges `a-b`, lists `a,b`, steps
 * `*\/n` and `a-b/n`, month names (JAN–DEC) and day names (SUN–SAT, 7 = Sunday).
 */

export interface CronField {
  /** Sorted, de-duplicated values the field matches. */
  values: number[];
  /** The field was `*` (or `*` with a step), i.e. not restricted. */
  wildcard: boolean;
  /** Step of `*\/n`, when the field is exactly that. */
  step?: number;
}

export interface Cron {
  minute: CronField;
  hour: CronField;
  dayOfMonth: CronField;
  month: CronField;
  dayOfWeek: CronField;
}

export type CronResult = { ok: true; cron: Cron } | { ok: false; error: string };

interface Spec {
  name: string;
  min: number;
  max: number;
  names?: string[];
}

const MONTHS = ['JAN', 'FEB', 'MAR', 'APR', 'MAY', 'JUN', 'JUL', 'AUG', 'SEP', 'OCT', 'NOV', 'DEC'];
const DAYS = ['SUN', 'MON', 'TUE', 'WED', 'THU', 'FRI', 'SAT'];

const SPECS: Spec[] = [
  { name: 'minute', min: 0, max: 59 },
  { name: 'hour', min: 0, max: 23 },
  { name: 'day of month', min: 1, max: 31 },
  { name: 'month', min: 1, max: 12, names: MONTHS },
  { name: 'day of week', min: 0, max: 7, names: DAYS },
];

export const DAY_NAMES = ['Sunday', 'Monday', 'Tuesday', 'Wednesday', 'Thursday', 'Friday', 'Saturday'];
const MONTH_NAMES = [
  'January', 'February', 'March', 'April', 'May', 'June',
  'July', 'August', 'September', 'October', 'November', 'December',
];

class CronError extends Error {}

function atom(raw: string, spec: Spec, offset: number): number {
  const up = raw.toUpperCase();
  const named = spec.names?.indexOf(up) ?? -1;
  if (named >= 0) return named + offset;
  if (!/^\d+$/.test(raw)) throw new CronError(`${spec.name}: "${raw}" is not a number`);
  const n = Number(raw);
  if (n < spec.min || n > spec.max) throw new CronError(`${spec.name}: ${n} is outside ${spec.min}–${spec.max}`);
  return n;
}

function parseField(text: string, spec: Spec): CronField {
  const offset = spec.names === MONTHS ? 1 : 0;
  const out = new Set<number>();
  let wildcard = false;
  let step: number | undefined;
  const parts = text.split(',');
  for (const part of parts) {
    if (part === '') throw new CronError(`${spec.name}: empty list item`);
    const [range, stepText, extra] = part.split('/');
    if (extra !== undefined) throw new CronError(`${spec.name}: "${part}" has more than one "/"`);
    let by = 1;
    if (stepText !== undefined) {
      if (!/^\d+$/.test(stepText) || Number(stepText) < 1) throw new CronError(`${spec.name}: step "${stepText}" must be a positive number`);
      by = Number(stepText);
    }
    let lo: number;
    let hi: number;
    if (range === '*') {
      lo = spec.min;
      hi = spec.name === 'day of week' ? 6 : spec.max;
      // Only a plain "*" leaves the field unrestricted ("*/2" restricts it, as in Go's cron parsers).
      if (parts.length === 1) {
        if (by === 1) wildcard = true;
        else step = by;
      }
    } else if (range!.includes('-')) {
      const [a, b, more] = range!.split('-');
      if (more !== undefined || !a || !b) throw new CronError(`${spec.name}: "${range}" is not a valid range`);
      lo = atom(a, spec, offset);
      hi = atom(b, spec, offset);
      if (lo > hi) throw new CronError(`${spec.name}: range "${range}" runs backwards`);
    } else {
      lo = atom(range!, spec, offset);
      hi = stepText !== undefined ? (spec.name === 'day of week' ? 6 : spec.max) : lo;
    }
    for (let v = lo; v <= hi; v += by) out.add(spec.name === 'day of week' && v === 7 ? 0 : v);
  }
  return { values: [...out].sort((a, b) => a - b), wildcard, step };
}

export function parseCron(expr: string): CronResult {
  const fields = expr.trim().split(/\s+/).filter(Boolean);
  if (fields.length === 0) return { ok: false, error: 'Enter a cron expression' };
  if (fields[0]!.startsWith('@')) return { ok: false, error: 'Use the five-field form, e.g. "0 3 * * *"' };
  if (fields.length !== 5) {
    return { ok: false, error: `Expected 5 fields (minute hour day month weekday), got ${fields.length}` };
  }
  try {
    const [minute, hour, dayOfMonth, month, dayOfWeek] = fields.map((f, i) => parseField(f, SPECS[i]!)) as [
      CronField, CronField, CronField, CronField, CronField,
    ];
    return { ok: true, cron: { minute, hour, dayOfMonth, month, dayOfWeek } };
  } catch (e) {
    if (e instanceof CronError) return { ok: false, error: e.message.charAt(0).toUpperCase() + e.message.slice(1) };
    throw e;
  }
}

const pad = (n: number) => String(n).padStart(2, '0');
const hhmm = (h: number, m: number) => `${pad(h)}:${pad(m)}`;

function joinWords(items: string[]): string {
  if (items.length <= 1) return items.join('');
  return `${items.slice(0, -1).join(', ')} and ${items[items.length - 1]}`;
}

/** Collapses consecutive values into ranges: [1,2,3,5] → [[1,3],[5,5]]. */
function runs(values: number[]): [number, number][] {
  const out: [number, number][] = [];
  for (const v of values) {
    const last = out[out.length - 1];
    if (last && v === last[1] + 1) last[1] = v;
    else out.push([v, v]);
  }
  return out;
}

function describeDays(c: Cron): string | null {
  const dom = c.dayOfMonth;
  const dow = c.dayOfWeek;
  const parts: string[] = [];
  if (!dow.wildcard) {
    const v = dow.values;
    if (v.join() === '1,2,3,4,5') parts.push('every weekday');
    else if (v.join() === '0,6') parts.push('every weekend day');
    else {
      parts.push(
        `every ${joinWords(
          runs(v).map(([a, b]) => (a === b ? DAY_NAMES[a]! : b === a + 1 ? `${DAY_NAMES[a]} and ${DAY_NAMES[b]}` : `${DAY_NAMES[a]} to ${DAY_NAMES[b]}`)),
        )}`,
      );
    }
  }
  if (!dom.wildcard) {
    const days = runs(dom.values).map(([a, b]) => (a === b ? String(a) : `${a}–${b}`));
    parts.push(`on day ${joinWords(days)} of the month`);
  }
  if (parts.length === 0) return null;
  // Standard cron semantics: with both restricted, either one matching is enough.
  return parts.join(' or ');
}

function describeMonths(c: Cron): string | null {
  if (c.month.wildcard) return null;
  return `in ${joinWords(runs(c.month.values).map(([a, b]) => (a === b ? MONTH_NAMES[a - 1]! : `${MONTH_NAMES[a - 1]} to ${MONTH_NAMES[b - 1]}`)))}`;
}

function describeTime(c: Cron): string {
  const { minute: m, hour: h } = c;
  if (h.wildcard) {
    if (m.wildcard) return 'every minute';
    if (m.step) return `every ${m.step} minutes`;
    if (m.values.length === 1) return m.values[0] === 0 ? 'every hour' : `every hour at minute ${m.values[0]}`;
    return `every hour at minutes ${joinWords(m.values.map(String))}`;
  }
  const hourRuns = runs(h.values);
  if ((m.wildcard || m.step) && hourRuns.length === 1) {
    const [a, b] = hourRuns[0]!;
    const every = m.wildcard ? 'every minute' : `every ${m.step} minutes`;
    return `${every} between ${hhmm(a, 0)} and ${hhmm(b, 59)} UTC`;
  }
  if (h.step && m.values.length === 1) {
    return m.values[0] === 0 ? `every ${h.step} hours` : `every ${h.step} hours at minute ${m.values[0]}`;
  }
  const times = h.values.flatMap((hh) => m.values.map((mm) => hhmm(hh, mm)));
  if (times.length > 6) return `${times.length} times a day`;
  return `at ${joinWords(times)} UTC`;
}

/**
 * Plain-language form of a valid expression, e.g. "Every day at 03:00 UTC".
 * Returns null for an invalid one.
 */
export function describeCron(expr: string): string | null {
  const r = parseCron(expr);
  if (!r.ok) return null;
  const c = r.cron;
  const time = describeTime(c);
  const days = describeDays(c);
  const months = describeMonths(c);
  let text: string;
  if (!days) text = time.startsWith('at ') ? `every day ${time}` : time;
  else text = time.startsWith('at ') ? `${days} ${time}` : `${time}, ${days}`;
  if (months) text = `${text}, ${months}`;
  return text.charAt(0).toUpperCase() + text.slice(1);
}

function matchesDay(c: Cron, d: Date): boolean {
  if (!c.month.values.includes(d.getUTCMonth() + 1)) return false;
  const domOk = c.dayOfMonth.values.includes(d.getUTCDate());
  const dowOk = c.dayOfWeek.values.includes(d.getUTCDay());
  if (c.dayOfMonth.wildcard && c.dayOfWeek.wildcard) return true;
  if (c.dayOfMonth.wildcard) return dowOk;
  if (c.dayOfWeek.wildcard) return domOk;
  return domOk || dowOk;
}

/** The next `count` run times strictly after `from` (UTC); fewer if none within ~5 years. */
export function nextRuns(expr: string, from: Date, count = 3): Date[] {
  const r = parseCron(expr);
  if (!r.ok) return [];
  const c = r.cron;
  const out: Date[] = [];
  const day = new Date(Date.UTC(from.getUTCFullYear(), from.getUTCMonth(), from.getUTCDate()));
  for (let i = 0; i < 366 * 5 && out.length < count; i++) {
    if (matchesDay(c, day)) {
      for (const h of c.hour.values) {
        for (const m of c.minute.values) {
          const t = new Date(day.getTime() + (h * 60 + m) * 60_000);
          if (t.getTime() > from.getTime()) {
            out.push(t);
            if (out.length === count) return out;
          }
        }
      }
    }
    day.setUTCDate(day.getUTCDate() + 1);
  }
  return out;
}

/* ---------------------------------------------------------------- schedule builder */

export type ScheduleMode = 'manual' | 'hourly' | 'daily' | 'weekly' | 'custom';

export interface ScheduleDraft {
  mode: ScheduleMode;
  /** "HH:MM" for daily and weekly. */
  time: string;
  /** 0 (Sunday) – 6 for weekly. */
  weekday: number;
  /** Minute past the hour for hourly. */
  minute: number;
  /** Free-form expression for custom. */
  cron: string;
}

export const DEFAULT_SCHEDULE: ScheduleDraft = { mode: 'daily', time: '03:00', weekday: 0, minute: 0, cron: '0 3 * * *' };

function parseTime(t: string): [number, number] | null {
  const m = /^(\d{1,2}):(\d{2})$/.exec(t.trim());
  if (!m) return null;
  const h = Number(m[1]);
  const min = Number(m[2]);
  return h <= 23 && min <= 59 ? [h, min] : null;
}

/** The cron expression a draft stands for; null = manual only; undefined = invalid. */
export function scheduleToCron(d: ScheduleDraft): string | null | undefined {
  switch (d.mode) {
    case 'manual':
      return null;
    case 'hourly':
      return Number.isInteger(d.minute) && d.minute >= 0 && d.minute <= 59 ? `${d.minute} * * * *` : undefined;
    case 'daily':
    case 'weekly': {
      const t = parseTime(d.time);
      if (!t) return undefined;
      return d.mode === 'daily' ? `${t[1]} ${t[0]} * * *` : `${t[1]} ${t[0]} * * ${d.weekday}`;
    }
    case 'custom': {
      const expr = d.cron.trim().split(/\s+/).join(' ');
      return parseCron(expr).ok ? expr : undefined;
    }
  }
}

/** Picks the simplest builder mode that reproduces `expr` exactly. */
export function cronToSchedule(expr: string | null | undefined): ScheduleDraft {
  if (!expr) return { ...DEFAULT_SCHEDULE, mode: 'manual' };
  const norm = expr.trim().split(/\s+/).join(' ');
  const base = { ...DEFAULT_SCHEDULE, cron: norm };
  let m = /^(\d{1,2}) \* \* \* \*$/.exec(norm);
  if (m && Number(m[1]) <= 59) return { ...base, mode: 'hourly', minute: Number(m[1]) };
  m = /^(\d{1,2}) (\d{1,2}) \* \* (\*|[0-7])$/.exec(norm);
  if (m && Number(m[1]) <= 59 && Number(m[2]) <= 23) {
    const time = hhmm(Number(m[2]), Number(m[1]));
    if (m[3] === '*') return { ...base, mode: 'daily', time };
    return { ...base, mode: 'weekly', time, weekday: Number(m[3]) % 7 };
  }
  return { ...base, mode: 'custom' };
}
