import { describe, expect, it } from 'vitest';
import { formatBytes, formatCompact, formatSignedPercent, greeting, initials, relativeTime, splitBytes } from './format';
import { clientSnippet } from './clientSnippets';

describe('format helpers', () => {
  it('formats bytes with SI units', () => {
    expect(splitBytes(4.82e12)).toEqual({ value: '4.82', unit: 'TB' });
    expect(formatBytes(12e12)).toBe('12 TB');
    expect(formatBytes(812e9)).toBe('812 GB');
    expect(formatBytes(2.1e12)).toBe('2.1 TB');
    expect(formatBytes(0)).toBe('0 B');
  });

  it('formats counts, trends, names and times', () => {
    expect(formatCompact(18_400_000)).toBe('18.4M');
    expect(formatCompact(950)).toBe('950');
    expect(formatSignedPercent(6)).toBe('+6%');
    expect(formatSignedPercent(-2.4)).toBe('−2%');
    expect(initials('Dilshod Ismoilov')).toBe('DI');
    const now = new Date('2026-11-03T09:00:00');
    expect(greeting(now)).toBe('Good morning');
    expect(relativeTime('2026-11-03T08:59:30', now)).toBe('Just now');
    expect(relativeTime('2026-11-03T08:56:00', now)).toBe('4 min ago');
  });

  it('builds client setup lines from endpoints', () => {
    const lines = clientSnippet([{ format: 'npm', endpoint: 'https://pkg.example/npm', repository: 'npm-group' }]);
    expect(lines).toEqual([{ text: '# npm', comment: true }, { text: 'npm config set registry https://pkg.example/npm/' }]);
  });
});
