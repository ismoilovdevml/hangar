import { describe, expect, it } from 'vitest';
import type { PackagePlan } from '../api/types';
import {
  advisoryIds,
  findingsSummary,
  findingStatus,
  lineFor,
  planQuery,
  sortedAliases,
  upgradeCommands,
  upgradeTargets,
  validateVulnSearch,
  validateWaiver,
  waiverUntil,
} from './vulnerabilities';

describe('vulnerability helpers', () => {
  it('reads only valid filters from the URL', () => {
    expect(validateVulnSearch({ tab: 'waived', mine: 'true', repo: ' nuget-proxy ', q: '', open: 'nuget:Newtonsoft.Json', finding: 'f1' })).toEqual({
      tab: 'waived',
      mine: true,
      repo: 'nuget-proxy',
      q: undefined,
      open: ['nuget:Newtonsoft.Json'],
      finding: 'f1',
    });
    expect(validateVulnSearch({ tab: 'in_use', mine: 'no', open: [] })).toEqual({
      tab: undefined,
      mine: undefined,
      repo: undefined,
      q: undefined,
      open: undefined,
      finding: undefined,
    });
  });

  it('builds the API query', () => {
    expect(planQuery({})).toEqual({ tab: 'in_use', window: '7d', assignee: undefined, repository: undefined, q: undefined });
    expect(planQuery({ tab: 'waived', mine: true, q: 'json' })).toMatchObject({ tab: 'waived', assignee: 'me', q: 'json' });
  });

  it('words a package plan', () => {
    const line = (from: string[], to: string | null) => ({
      line: from[0]!.split('.')[0]!,
      from,
      to,
      command: to ? `dotnet add package System.Security.Cryptography.Xml --version ${to}` : null,
      fixes: 1,
      unfixed: to ? 0 : 1,
    });
    const p = {
      advisories: [
        { id: 'GHSA-g8r8-53c2-pm3f', aliases: ['CVE-2026-47304'], severity: 'high', cvss: 8.1 },
        { id: 'GHSA-x', aliases: [], severity: 'medium', cvss: 5 },
      ],
      upgrades: [line(['8.0.2'], '8.0.4'), line(['10.0.8'], '10.0.10'), line(['9.0.1'], '10.0.10')],
      findings: 3,
      max_severity: 'high',
      max_cvss: 8.1,
      severities: { critical: 0, high: 2, medium: 1, low: 0, unknown: 0 },
    } as unknown as PackagePlan;
    expect(upgradeTargets(p)).toBe('8.0.4 / 10.0.10');
    expect(upgradeCommands(p).split('\n')).toHaveLength(2);
    expect(lineFor(p, '10.0.8')?.to).toBe('10.0.10');
    expect(advisoryIds(p)).toBe('GHSA-g8r8-53c2-pm3f · CVE-2026-47304 + 1 more');
    expect(findingsSummary(p)).toBe('2 high + 1 · 8.1');
    expect(upgradeTargets({ upgrades: [line(['1.0.0'], null)] })).toBeNull();
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

  it('puts CVE aliases first', () => {
    expect(sortedAliases(['PYSEC-1', 'CVE-2026-2', 'GHSA-x'])).toEqual(['CVE-2026-2', 'GHSA-x', 'PYSEC-1']);
  });
});
