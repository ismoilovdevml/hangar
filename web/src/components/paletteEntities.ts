/**
 * Live results of the command palette: repositories, packages, advisories
 * and users that match what was typed. Each source is asked only when the
 * caller may read it; a failed source just shows nothing.
 */
import { useEffect, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { apiFetch } from '../api/client';
import type { FindingList, RepositoryList, SearchResult } from '../api/types';
import type { MemberPage } from '../pages/access/api';

/** Advisory ids people paste: CVE-2021-44228, GHSA-jfh8-c2jp-5v3q, PYSEC-…, RUSTSEC-…. */
export const ADVISORY_RE = /^(cve-\d{4}-\d{4,}|ghsa(-[23456789cfghjmpqrvwx]{4}){3}|pysec-\d{4}-\d+|rustsec-\d{4}-\d{4})$/i;

export const ENTITY_LIMIT = 5;
const MIN_LENGTH = 2;

function useDebounced(value: string, ms: number): string {
  const [debounced, setDebounced] = useState(value);
  useEffect(() => {
    const t = setTimeout(() => setDebounced(value), ms);
    return () => clearTimeout(t);
  }, [value, ms]);
  return debounced;
}

const quiet = { retry: false, staleTime: 30_000 } as const;

export function usePaletteEntities(query: string, { users }: { users: boolean }) {
  const q = useDebounced(query.trim(), 200);
  const on = q.length >= MIN_LENGTH;
  const advisory = ADVISORY_RE.test(q);

  const repositories = useQuery({
    queryKey: ['palette', 'repositories', q],
    queryFn: ({ signal }) => apiFetch<RepositoryList>('/repositories', { query: { q, limit: ENTITY_LIMIT }, signal }),
    enabled: on && !advisory,
    ...quiet,
  });
  const packages = useQuery({
    queryKey: ['palette', 'search', q],
    queryFn: ({ signal }) => apiFetch<SearchResult>('/search', { query: { q, limit: ENTITY_LIMIT }, signal }),
    enabled: on,
    ...quiet,
  });
  const findings = useQuery({
    queryKey: ['palette', 'findings', q],
    queryFn: ({ signal }) => apiFetch<FindingList>('/findings', { query: { q, limit: ENTITY_LIMIT }, signal }),
    enabled: on && advisory,
    ...quiet,
  });
  const members = useQuery({
    queryKey: ['palette', 'users', q],
    queryFn: ({ signal }) => apiFetch<MemberPage>('/users', { query: { q, limit: ENTITY_LIMIT }, signal }),
    enabled: on && users && !advisory,
    ...quiet,
  });

  return {
    /** The query the results belong to (debounced). */
    query: on ? q : '',
    repositories: (on && !advisory && repositories.data?.items) || [],
    packages: (on && packages.data?.items) || [],
    findings: (on && advisory && findings.data?.items) || [],
    users: (on && users && !advisory && members.data?.items) || [],
  };
}
