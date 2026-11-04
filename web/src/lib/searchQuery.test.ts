import { describe, expect, it } from 'vitest';
import {
  applySuggestion,
  freeText,
  highlight,
  highlightTerms,
  normalizeQuery,
  qualifierValues,
  setQualifier,
  suggest,
  toggleQualifierValue,
  tokenize,
} from './searchQuery';

const ctx = { formats: ['docker', 'npm', 'nuget'], repositories: ['npm-hosted', 'npm-proxy', 'docker-prod'], today: new Date('2026-11-04T10:00:00Z') };

describe('query tokens', () => {
  it('treats only known keys as qualifiers, so maven coordinates stay free text', () => {
    const toks = tokenize('com.acme:ledger format:maven FORMAT:npm');
    expect(toks.map((t) => t.key)).toEqual([undefined, 'format', 'format']);
    expect(freeText('com.acme:ledger format:maven')).toBe('com.acme:ledger');
    expect(qualifierValues('format:maven FORMAT:NPM', 'format')).toEqual(['maven', 'npm']);
  });

  it('sets, toggles and removes qualifier values without touching free text', () => {
    expect(setQualifier('log4j repo:a format:npm', 'repo', ['b'])).toBe('log4j format:npm repo:b');
    expect(setQualifier('log4j repo:a', 'repo', [])).toBe('log4j');
    expect(toggleQualifierValue('log4j', 'format', 'maven')).toBe('log4j format:maven');
    expect(toggleQualifierValue('log4j format:maven format:npm', 'format', 'maven')).toBe('log4j format:npm');
  });

  it('normalises whitespace', () => {
    expect(normalizeQuery('  a   format:npm ')).toBe('a format:npm');
  });
});

describe('highlighting', () => {
  it('marks every occurrence of the free-text pieces, longest first, case-insensitive', () => {
    const terms = highlightTerms('Ledger-co format:maven');
    expect(terms).toEqual(['ledger', 'co']);
    const segs = highlight('com.acme:ledger-core', terms);
    expect(segs.filter((s) => s.match).map((s) => s.text)).toEqual(['co', 'ledger', 'co']);
    expect(segs.map((s) => s.text).join('')).toBe('com.acme:ledger-core');
  });

  it('returns the text unchanged without terms', () => {
    expect(highlight('abc', [])).toEqual([{ text: 'abc', match: false }]);
  });
});

describe('suggestions', () => {
  it('completes qualifier keys and keeps the caret after the colon', () => {
    const s = suggest('log fo', 6, ctx);
    expect(s.map((x) => x.insert)).toEqual(['format:']);
    expect(applySuggestion('log fo', 6, s[0]!)).toEqual({ q: 'log format:', caret: 11 });
  });

  it('completes values from the server lists and adds a space', () => {
    expect(suggest('format:n', 8, ctx).map((x) => x.insert)).toEqual(['format:npm', 'format:nuget']);
    expect(suggest('repo:npm-', 9, ctx).map((x) => x.insert)).toEqual(['repo:npm-hosted', 'repo:npm-proxy']);
    expect(suggest('security:', 9, ctx).map((x) => x.insert)).toContain('security:findings');
    expect(suggest('published:', 10, ctx)[0]!.insert).toBe('published:<2026-11-04');
    const s = suggest('format:np x', 9, ctx)[0]!;
    expect(applySuggestion('format:np x', 9, s)).toEqual({ q: 'format:npm x', caret: 11 });
  });

  it('offers nothing between tokens, for complete values or free-text keys', () => {
    expect(suggest('a  b', 2, ctx)).toEqual([]);
    expect(suggest('format:npm', 10, ctx)).toEqual([]);
    expect(suggest('sha256:ab', 9, ctx)).toEqual([]);
  });
});
