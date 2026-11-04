import { describe, expect, it } from 'vitest';
import {
  findingQuery,
  findingStatus,
  sortedAliases,
  trendSummary,
  validateVulnSearch,
  validateWaiver,
  waiverUntil,
} from './vulnerabilities';

describe('vulnerability helpers', () => {
  it('reads only valid filters from the URL', () => {
    expect(validateVulnSearch({ severity: 'critical', status: 'waived', fix: 'yes', repo: ' npm-internal ', q: '', finding: 'f1' })).toEqual({
      severity: 'critical',
      status: 'waived',
      fix: 'yes',
      repo: 'npm-internal',
      q: undefined,
      cursor: undefined,
      finding: 'f1',
    });
    expect(validateVulnSearch({ severity: 'urgent', status: 'active', fix: 'maybe' })).toEqual({
      severity: undefined,
      status: undefined,
      fix: undefined,
      repo: undefined,
      q: undefined,
      cursor: undefined,
      finding: undefined,
    });
  });

  it('builds the API query', () => {
    expect(findingQuery({})).toMatchObject({ status: undefined, fix_available: undefined, limit: 50 });
    expect(findingQuery({ status: 'open', fix: 'no', severity: 'high' })).toMatchObject({ status: 'open,assigned', fix_available: false, severity: 'high' });
  });

  it('words the status, including expired waivers', () => {
    const now = new Date('2026-11-04T12:00:00Z');
    expect(findingStatus({ status: 'waived', waiver_until: '2026-12-04T23:59:59Z', quarantined: false }, now).label).toBe('Waived until 4 Dec 2026');
    expect(findingStatus({ status: 'open', waiver_until: '2026-11-01T23:59:59Z', quarantined: false }, now).label).toBe('Open · waiver expired 1 Nov 2026');
    expect(findingStatus({ status: 'open', waiver_until: null, quarantined: true }, now)).toEqual({ label: 'Open · quarantined', tone: 'bad' });
    expect(findingStatus({ status: 'fixed', waiver_until: null, quarantined: false }, now).tone).toBe('ok');
  });

  it('validates waivers like the server', () => {
    const now = new Date('2026-11-04T12:00:00Z');
    expect(validateWaiver('ok', '2026-12-01', now).reason).toBeDefined();
    expect(validateWaiver('Not reachable', '2026-11-03', now).until).toMatch(/future/);
    expect(validateWaiver('Not reachable', '2028-01-01', now).until).toMatch(/year/);
    expect(validateWaiver('Not reachable', 'nope', now).until).toBeDefined();
    expect(validateWaiver('Not reachable', '2026-11-04', now)).toEqual({});
    expect(waiverUntil('2026-11-04')!.toISOString()).toBe('2026-11-04T23:59:59.000Z');
  });

  it('puts CVE aliases first and summarises the trend', () => {
    expect(sortedAliases(['PYSEC-1', 'CVE-2026-2', 'GHSA-x'])).toEqual(['CVE-2026-2', 'GHSA-x', 'PYSEC-1']);
    expect(trendSummary([])).toBe('No finding history yet.');
    const p = (critical: number, high: number, n = 0, r = 0) => ({ date: '2026-11-01', critical, high, medium: 0, low: 0, new: n, resolved: r });
    expect(trendSummary([p(0, 2), p(1, 3, 2, 0)])).toBe('4 unresolved findings today (1 critical), up from 2 2 days ago; 2 detected and 0 fixed in that time.');
  });
});
