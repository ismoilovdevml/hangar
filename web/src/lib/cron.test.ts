import { describe, expect, it } from 'vitest';
import { cronToSchedule, describeCron, nextRuns, parseCron, scheduleToCron, DEFAULT_SCHEDULE } from './cron';

describe('parseCron', () => {
  it('accepts the common forms', () => {
    for (const e of ['0 3 * * *', '*/15 * * * *', '0 0 1 1 *', '30 2 * * MON-FRI', '0 4 1,15 * *', '0 0 * JAN,JUL 0', '5/10 * * * 7']) {
      expect(parseCron(e).ok, e).toBe(true);
    }
  });

  it('explains what is wrong', () => {
    expect(parseCron('')).toEqual({ ok: false, error: 'Enter a cron expression' });
    expect(parseCron('0 3 * *')).toMatchObject({ ok: false, error: expect.stringContaining('Expected 5 fields') });
    expect(parseCron('60 3 * * *')).toMatchObject({ ok: false, error: 'Minute: 60 is outside 0–59' });
    expect(parseCron('0 24 * * *')).toMatchObject({ ok: false, error: 'Hour: 24 is outside 0–23' });
    expect(parseCron('0 3 5-1 * *')).toMatchObject({ ok: false, error: expect.stringContaining('runs backwards') });
    expect(parseCron('0 3 * * FUNDAY')).toMatchObject({ ok: false, error: expect.stringContaining('not a number') });
    expect(parseCron('*/0 * * * *')).toMatchObject({ ok: false, error: expect.stringContaining('positive number') });
    expect(parseCron('@daily')).toMatchObject({ ok: false, error: expect.stringContaining('five-field') });
  });

  it('expands steps, ranges, names and Sunday as 7', () => {
    const r = parseCron('*/20 9-11 * FEB 7');
    if (!r.ok) throw new Error(r.error);
    expect(r.cron.minute.values).toEqual([0, 20, 40]);
    expect(r.cron.minute.step).toBe(20);
    expect(r.cron.minute.wildcard).toBe(false);
    expect(r.cron.hour.values).toEqual([9, 10, 11]);
    expect(r.cron.month.values).toEqual([2]);
    expect(r.cron.dayOfWeek.values).toEqual([0]);
    expect(r.cron.dayOfMonth.wildcard).toBe(true);
  });
});

describe('describeCron', () => {
  it.each([
    ['0 3 * * *', 'Every day at 03:00 UTC'],
    ['0 2 * * *', 'Every day at 02:00 UTC'],
    ['0 3 * * 0', 'Every Sunday at 03:00 UTC'],
    ['0 3 * * 7', 'Every Sunday at 03:00 UTC'],
    ['30 1 * * 1-5', 'Every weekday at 01:30 UTC'],
    ['0 6 * * 6,0', 'Every weekend day at 06:00 UTC'],
    ['0 0 * * 1,3', 'Every Monday, Wednesday at 00:00 UTC'.replace(', W', ' and W')],
    ['0 0 * * 1-3', 'Every Monday to Wednesday at 00:00 UTC'],
    ['0 */6 * * *', 'Every 6 hours'],
    ['15 */4 * * *', 'Every 4 hours at minute 15'],
    ['*/15 * * * *', 'Every 15 minutes'],
    ['* * * * *', 'Every minute'],
    ['0 * * * *', 'Every hour'],
    ['30 * * * *', 'Every hour at minute 30'],
    ['0 2,14 * * *', 'Every day at 02:00 and 14:00 UTC'],
    ['0 1 1 * *', 'On day 1 of the month at 01:00 UTC'],
    ['0 1 1,15 * *', 'On day 1 and 15 of the month at 01:00 UTC'],
    ['0 0 1 1 *', 'On day 1 of the month at 00:00 UTC, in January'],
    ['*/10 9-17 * * 1-5', 'Every 10 minutes between 09:00 and 17:59 UTC, every weekday'],
  ])('%s → %s', (expr, text) => {
    expect(describeCron(expr)).toBe(text);
  });

  it('returns null for an invalid expression', () => {
    expect(describeCron('nope')).toBeNull();
  });
});

describe('nextRuns', () => {
  const from = new Date('2026-11-04T10:30:00Z'); // a Wednesday

  it('finds the next daily runs in UTC', () => {
    expect(nextRuns('0 3 * * *', from, 2).map((d) => d.toISOString())).toEqual([
      '2026-11-05T03:00:00.000Z',
      '2026-11-06T03:00:00.000Z',
    ]);
  });

  it('includes a later run on the same day and skips the current minute', () => {
    expect(nextRuns('30 10,12 * * *', from, 1)[0]!.toISOString()).toBe('2026-11-04T12:30:00.000Z');
  });

  it('honours weekdays and month boundaries', () => {
    expect(nextRuns('0 3 * * 0', from, 1)[0]!.toISOString()).toBe('2026-11-08T03:00:00.000Z');
    expect(nextRuns('0 0 1 * *', from, 1)[0]!.toISOString()).toBe('2026-12-01T00:00:00.000Z');
  });

  it('matches either day field when both are restricted', () => {
    // 13th of the month or a Friday.
    const runs = nextRuns('0 0 13 * 5', from, 3).map((d) => d.toISOString().slice(0, 10));
    expect(runs).toEqual(['2026-11-06', '2026-11-13', '2026-11-20']);
  });

  it('gives up on impossible dates', () => {
    expect(nextRuns('0 0 31 2 *', from, 1)).toEqual([]);
    expect(nextRuns('bad', from)).toEqual([]);
  });
});

describe('schedule builder', () => {
  it('builds cron from each mode', () => {
    expect(scheduleToCron({ ...DEFAULT_SCHEDULE, mode: 'manual' })).toBeNull();
    expect(scheduleToCron({ ...DEFAULT_SCHEDULE, mode: 'hourly', minute: 5 })).toBe('5 * * * *');
    expect(scheduleToCron({ ...DEFAULT_SCHEDULE, mode: 'daily', time: '02:30' })).toBe('30 2 * * *');
    expect(scheduleToCron({ ...DEFAULT_SCHEDULE, mode: 'weekly', time: '03:00', weekday: 0 })).toBe('0 3 * * 0');
    expect(scheduleToCron({ ...DEFAULT_SCHEDULE, mode: 'custom', cron: ' 0  */6 * * * ' })).toBe('0 */6 * * *');
  });

  it('flags invalid input as undefined', () => {
    expect(scheduleToCron({ ...DEFAULT_SCHEDULE, mode: 'daily', time: '25:00' })).toBeUndefined();
    expect(scheduleToCron({ ...DEFAULT_SCHEDULE, mode: 'custom', cron: '0 3 * *' })).toBeUndefined();
    expect(scheduleToCron({ ...DEFAULT_SCHEDULE, mode: 'hourly', minute: 75 })).toBeUndefined();
  });

  it('round-trips through the simplest mode', () => {
    expect(cronToSchedule(null).mode).toBe('manual');
    expect(cronToSchedule('7 * * * *')).toMatchObject({ mode: 'hourly', minute: 7 });
    expect(cronToSchedule('0 2 * * *')).toMatchObject({ mode: 'daily', time: '02:00' });
    expect(cronToSchedule('30 4 * * 7')).toMatchObject({ mode: 'weekly', time: '04:30', weekday: 0 });
    expect(cronToSchedule('0 */6 * * *')).toMatchObject({ mode: 'custom', cron: '0 */6 * * *' });
    for (const e of ['7 * * * *', '0 2 * * *', '30 4 * * 3', '0 */6 * * *']) {
      expect(scheduleToCron(cronToSchedule(e))).toBe(e);
    }
  });
});
