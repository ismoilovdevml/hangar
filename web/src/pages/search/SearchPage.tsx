import { useEffect, useId, useMemo, useRef, type KeyboardEvent } from 'react';
import { getRouteApi, Link } from '@tanstack/react-router';
import { ApiRequestError } from '../../api/client';
import { flatItems, useAllRepositories, useSearch } from '../../api/catalog';
import { useFormats } from '../../api/hooks';
import type { FacetValue, SearchHit, SearchResult, SearchSort } from '../../api/types';
import { ErrorNote } from '../../components/ErrorNote';
import { FormatBadge } from '../../components/FormatBadge';
import { Pill } from '../../components/Pill';
import { Select } from '../../components/Select';
import { FORMAT_META, formatCompact, relativeTime } from '../../lib/format';
import { hasVersion, hitName, securityPill } from '../../lib/packages';
import { isFormat } from '../../lib/repositories';
import {
  highlight,
  highlightTerms,
  normalizeQuery,
  QUALIFIERS,
  qualifierValues,
  SEARCH_SORTS,
  setQualifier,
  toggleQualifierValue,
  type SecurityState,
} from '../../lib/searchQuery';
import { useNow } from '../../lib/useNow';
import { QueryBar, type QueryBarHandle } from './QueryBar';
import './search.css';

const route = getRouteApi('/app/search');

const EXAMPLES = ['format:npm', 'repo:npm-hosted', 'sha256:9f2c', 'unused:>90d', 'security:findings'];

const SECURITY_LABEL: Record<SecurityState, string> = {
  clean: 'Clean',
  findings: 'Has findings',
  quarantined: 'Quarantined',
  pending: 'Not scanned',
};

export function SearchPage() {
  const search = route.useSearch();
  const navigate = route.useNavigate();
  const q = search.q ?? '';
  const sort: SearchSort = search.sort ?? 'relevance';
  const formats = useFormats();
  const repos = useAllRepositories();
  const results = useSearch(q, sort);
  const bar = useRef<QueryBarHandle>(null);
  const list = useRef<HTMLOListElement>(null);

  const setQuery = (next: string, replace = false) =>
    void navigate({ search: (s) => ({ ...s, q: normalizeQuery(next) || undefined }), replace });

  // "/" focuses the search box from anywhere on the page, as on most search screens.
  useEffect(() => {
    const onKey = (e: globalThis.KeyboardEvent) => {
      if (e.key !== '/' || e.metaKey || e.ctrlKey || e.altKey) return;
      const t = e.target as HTMLElement | null;
      if (t && (t.isContentEditable || ['INPUT', 'TEXTAREA', 'SELECT'].includes(t.tagName))) return;
      e.preventDefault();
      bar.current?.focus();
    };
    document.addEventListener('keydown', onKey);
    return () => document.removeEventListener('keydown', onKey);
  }, []);

  const context = useMemo(
    () => ({
      formats: (formats.data?.items ?? []).map((f) => f.name),
      repositories: (repos.data ?? []).map((r) => r.name).sort(),
    }),
    [formats.data, repos.data],
  );

  const focusResult = (i: number) => list.current?.querySelectorAll<HTMLElement>('.result__link')[i]?.focus();

  const first = results.data?.pages[0];
  const hits = flatItems(results.data);
  const invalid = results.error instanceof ApiRequestError && results.error.status === 400 ? results.error : null;

  return (
    <div className="screen search-screen">
      <section className="search-hero glass" aria-labelledby="search-title">
        <h1 id="search-title" className="search-hero__title">
          Find any package
        </h1>
        <QueryBar ref={bar} value={q} onSubmit={(v) => setQuery(v)} context={context} onArrowDown={() => focusResult(0)} />
        <div className="search-try">
          <span className="meta">Try:</span>
          {EXAMPLES.map((ex) => (
            <Link key={ex} to="/search" search={{ q: ex }} className="search-try__chip">
              {ex}
            </Link>
          ))}
        </div>
      </section>

      {!q ? (
        <QueryHelp />
      ) : (
        <div className="search-body">
          {first && <Facets q={q} result={first} onChange={(next) => setQuery(next, false)} />}
          <section className="search-results" aria-labelledby="results-title">
            <div className="search-results__head">
              <h2 id="results-title" className="search-results__count" aria-live="polite">
                {invalid ? (
                  'Check the query'
                ) : first ? (
                  <>
                    <strong>{first.total.toLocaleString('en-GB')}</strong> {first.total === 1 ? 'package' : 'packages'} for “{q}”
                  </>
                ) : results.isError ? (
                  'Search failed'
                ) : (
                  'Searching…'
                )}
              </h2>
              <SortSelect value={sort} onChange={(s) => void navigate({ search: (p) => ({ ...p, sort: s === 'relevance' ? undefined : s }), replace: true })} />
            </div>

            {invalid ? (
              <p className="form-error" role="alert">
                {invalid.body?.message ?? 'The query is not valid.'}
              </p>
            ) : results.isError ? (
              <ErrorNote what="search results" onRetry={() => void results.refetch()} />
            ) : first && hits.length === 0 ? (
              <NoResults q={q} onClear={(next) => setQuery(next)} />
            ) : (
              <ResultList hits={hits} q={q} listRef={list} onEscape={() => bar.current?.focus()} busy={results.isFetching && !results.isFetchingNextPage} />
            )}

            {results.hasNextPage && (
              <button
                type="button"
                className="button button--secondary search-more"
                disabled={results.isFetchingNextPage}
                onClick={() => void results.fetchNextPage()}
              >
                {results.isFetchingNextPage ? 'Loading…' : `Show more (${hits.length} of ${first?.total ?? hits.length})`}
              </button>
            )}
          </section>
        </div>
      )}
    </div>
  );
}

function SortSelect({ value, onChange }: { value: SearchSort; onChange: (s: SearchSort) => void }) {
  const id = useId();
  return (
    <div className="filter-select">
      <label htmlFor={id} className="filter-select__label">
        Sort
      </label>
      <Select<SearchSort> id={id} variant="inline" value={value} options={SEARCH_SORTS.map((s) => ({ value: s.id, label: s.label }))} onChange={onChange} />
    </div>
  );
}

function ResultList({
  hits,
  q,
  listRef,
  onEscape,
  busy,
}: {
  hits: SearchHit[];
  q: string;
  listRef: React.RefObject<HTMLOListElement>;
  onEscape: () => void;
  busy: boolean;
}) {
  const terms = highlightTerms(q);
  const now = useNow();
  const onKeyDown = (e: KeyboardEvent<HTMLOListElement>) => {
    const links = [...(listRef.current?.querySelectorAll<HTMLElement>('.result__link') ?? [])];
    const i = links.indexOf(document.activeElement as HTMLElement);
    if (i < 0) return;
    let next = -1;
    if (e.key === 'ArrowDown') next = Math.min(links.length - 1, i + 1);
    else if (e.key === 'ArrowUp') {
      if (i === 0) {
        e.preventDefault();
        onEscape();
        return;
      }
      next = i - 1;
    } else if (e.key === 'Home') next = 0;
    else if (e.key === 'End') next = links.length - 1;
    else if (e.key === 'Escape') {
      e.preventDefault();
      onEscape();
      return;
    }
    if (next < 0) return;
    e.preventDefault();
    links[next]?.focus();
  };

  return (
    <ol ref={listRef} className="result-list" aria-label="Search results" aria-busy={busy || undefined} onKeyDown={onKeyDown}>
      {hits.map((h) => {
        const name = hitName(h);
        const pill = securityPill(h.security);
        return (
          <li key={h.component_id} className="result glass">
            <Link
              to="/browse"
              search={{ repo: h.repository, ns: h.namespace || undefined, name: h.name, version: h.version || undefined }}
              className="result__link"
            >
              {isFormat(h.format) && <FormatBadge format={h.format} size={40} />}
              <span className="result__text">
                <span className="result__name">
                  {highlight(name, terms).map((s, i) => (s.match ? <mark key={i}>{s.text}</mark> : <span key={i}>{s.text}</span>))}
                </span>
                <span className="result__meta">
                  {h.repository}
                  {hasVersion(h.version) && ` · ${h.version} newest`}
                  {` · ${h.versions} ${h.versions === 1 ? 'version' : 'versions'}`}
                  {` · ${formatCompact(h.downloads)} ${h.downloads === 1 ? 'download' : 'downloads'}`}
                  {h.published_at && ` · ${relativeTime(h.published_at, now).toLowerCase()}`}
                </span>
              </span>
              <Pill tone={pill.tone} className="result__pill">
                {pill.label}
              </Pill>
            </Link>
          </li>
        );
      })}
    </ol>
  );
}

function FacetCheckbox({ label, count, checked, onChange }: { label: string; count: number; checked: boolean; onChange: () => void }) {
  return (
    <label className="facet-option">
      <input type="checkbox" checked={checked} onChange={onChange} />
      <span className="facet-option__label">{label}</span>
      <span className="facet-option__count">{count}</span>
    </label>
  );
}

/** Facets edit the query string itself, so the box, the URL and the counts stay in step. */
function Facets({ q, result, onChange }: { q: string; result: SearchResult; onChange: (q: string) => void }) {
  const repoId = useId();
  const selectedFormats = qualifierValues(q, 'format');
  const selectedSecurity = qualifierValues(q, 'security');
  const selectedRepos = qualifierValues(q, 'repo');
  const withSelected = (facet: FacetValue[], selected: string[]) => [
    ...facet,
    ...selected.filter((v) => !facet.some((f) => f.value === v)).map((value) => ({ value, count: 0 })),
  ];
  const formats = withSelected(result.facets.format, selectedFormats);
  const security = withSelected(result.facets.security, selectedSecurity);
  const repos = withSelected(result.facets.repository, selectedRepos);
  const security1 = selectedSecurity.length === 1 ? selectedSecurity[0] : selectedSecurity.length ? 'mixed' : '';

  return (
    <aside className="facets glass" aria-label="Filters">
      <fieldset className="facet">
        <legend className="facet__title">Format</legend>
        {formats.length === 0 && <p className="meta">None</p>}
        {formats.map((f) => (
          <FacetCheckbox
            key={f.value}
            label={isFormat(f.value) ? FORMAT_META[f.value].label : f.value}
            count={f.count}
            checked={selectedFormats.includes(f.value)}
            onChange={() => onChange(toggleQualifierValue(q, 'format', f.value))}
          />
        ))}
      </fieldset>

      <fieldset className="facet">
        <legend className="facet__title">Security</legend>
        <label className="facet-option">
          <input type="radio" name="facet-security" checked={!security1} onChange={() => onChange(setQualifier(q, 'security', []))} />
          <span className="facet-option__label">Any</span>
        </label>
        {security.map((s) => (
          <label key={s.value} className="facet-option">
            <input type="radio" name="facet-security" checked={security1 === s.value} onChange={() => onChange(setQualifier(q, 'security', [s.value]))} />
            <span className="facet-option__label">{SECURITY_LABEL[s.value as SecurityState] ?? s.value}</span>
            <span className="facet-option__count">{s.count}</span>
          </label>
        ))}
      </fieldset>

      <div className="facet">
        <label htmlFor={repoId} className="facet__title">
          Repository
        </label>
        <Select
          id={repoId}
          size="sm"
          value={selectedRepos.length === 1 ? selectedRepos[0]! : ''}
          placeholder={selectedRepos.length > 1 ? `${selectedRepos.length} repositories` : 'All repositories'}
          options={[{ value: '', label: 'All repositories' }, ...repos.map((r) => ({ value: r.value, label: r.value, hint: `${r.count} ${r.count === 1 ? 'result' : 'results'}` }))]}
          onChange={(v) => onChange(setQualifier(q, 'repo', v ? [v] : []))}
        />
      </div>
    </aside>
  );
}

function NoResults({ q, onClear }: { q: string; onClear: (q: string) => void }) {
  const filters = QUALIFIERS.filter((k) => qualifierValues(q, k.key).length > 0);
  return (
    <div className="search-empty glass" role="status">
      <p className="search-empty__title">Nothing matches “{q}”.</p>
      <ul className="search-empty__tips">
        <li>Free-text words match the start of name parts: “ledg” finds com.acme:ledger-core.</li>
        <li>Packages in repositories you cannot read are not shown.</li>
        {filters.length > 0 && (
          <li>
            Remove a filter:{' '}
            {filters.map((k) => (
              <button key={k.key} type="button" className="search-try__chip" onClick={() => onClear(setQualifier(q, k.key, []))}>
                {k.key}:
              </button>
            ))}
          </li>
        )}
      </ul>
    </div>
  );
}

function QueryHelp() {
  return (
    <section className="search-help glass" aria-labelledby="help-title">
      <h2 id="help-title" className="panel__title">
        What you can search for
      </h2>
      <p className="meta">
        Words match the start of any part of a package name. Combine them with qualifiers; repeated qualifiers of the same kind mean “any of”. Press{' '}
        <kbd className="kbd">/</kbd> to jump to the search box.
      </p>
      <dl className="search-help__list">
        {QUALIFIERS.map((k) => (
          <div key={k.key} className="search-help__row">
            <dt>
              <Link to="/search" search={{ q: k.example }} className="search-try__chip">
                {k.example}
              </Link>
            </dt>
            <dd>{k.hint}</dd>
          </div>
        ))}
      </dl>
    </section>
  );
}
