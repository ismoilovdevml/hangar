import { useId, useMemo, useState } from 'react';
import { Link, useNavigate } from '@tanstack/react-router';
import { flatItems, useAllRepositories, useComponentLookup, useRepositoryPackages } from '../../api/catalog';
import type { SearchHit } from '../../api/types';
import { ErrorNote } from '../../components/ErrorNote';
import { FormatBadge } from '../../components/FormatBadge';
import { isFormat } from '../../lib/repositories';
import { hasVersion, hitName } from '../../lib/packages';

interface PackageTreeProps {
  repo?: string;
  ns?: string;
  name?: string;
  version?: string;
}

const sameNs = (a: string | undefined, b: string | undefined) => (a ?? '') === (b ?? '');

/** Left column: repository picker, then the repository's packages; the selected one expands to its versions. */
export function PackageTree({ repo, ns, name, version }: PackageTreeProps) {
  const navigate = useNavigate();
  const repos = useAllRepositories();
  const selectId = useId();
  const filterId = useId();
  const [filter, setFilter] = useState('');
  const sorted = useMemo(() => [...(repos.data ?? [])].sort((a, b) => a.name.localeCompare(b.name)), [repos.data]);
  const current = sorted.find((r) => r.name === repo);

  return (
    <nav className="browse-tree glass" aria-label="Repositories and packages">
      <div className="field">
        <label htmlFor={selectId} className="field__label">
          Repository
        </label>
        <select
          id={selectId}
          className="input"
          value={repo ?? ''}
          disabled={repos.isPending}
          onChange={(e) => {
            setFilter('');
            void navigate({ to: '/browse', search: e.target.value ? { repo: e.target.value } : {} });
          }}
        >
          <option value="">{repos.isPending ? 'Loading…' : 'Choose a repository'}</option>
          {repo && !current && repos.data && <option value={repo}>{repo}</option>}
          {sorted.map((r) => (
            <option key={r.id} value={r.name}>
              {r.name}
            </option>
          ))}
        </select>
      </div>

      {repos.isError && <ErrorNote what="repositories" onRetry={() => void repos.refetch()} />}

      {!repo && repos.data && (
        <ul className="tree-list" aria-label="Repositories">
          {sorted.length === 0 && <li className="meta tree-list__empty">No repositories yet.</li>}
          {sorted.map((r) => (
            <li key={r.id}>
              <Link to="/browse" search={{ repo: r.name }} className="tree-item">
                {isFormat(r.format) && <FormatBadge format={r.format} size={28} />}
                <span className="tree-item__name">{r.name}</span>
                <span className="tree-item__count">{r.type === 'group' ? 'group' : r.component_count}</span>
              </Link>
            </li>
          ))}
        </ul>
      )}

      {repo && current?.type === 'group' && (
        <div className="tree-group">
          <p className="meta">A group serves the packages of its members:</p>
          <ul className="tree-list">
            {(current.config.group?.members ?? []).map((m) => (
              <li key={m}>
                <Link to="/browse" search={{ repo: m }} className="tree-item">
                  <span className="tree-item__name">{m}</span>
                </Link>
              </li>
            ))}
          </ul>
        </div>
      )}

      {repo && current?.type !== 'group' && (
        <>
          <div className="field">
            <label htmlFor={filterId} className="visually-hidden">
              Filter packages
            </label>
            <input
              id={filterId}
              className="input tree-filter"
              type="search"
              placeholder="Filter packages"
              autoComplete="off"
              spellCheck={false}
              value={filter}
              onChange={(e) => setFilter(e.target.value)}
            />
          </div>
          <Packages repo={repo} ns={ns} name={name} version={version} filter={filter.trim().toLowerCase()} />
        </>
      )}
    </nav>
  );
}

function Packages({ repo, ns, name, version, filter }: PackageTreeProps & { repo: string; filter: string }) {
  const q = useRepositoryPackages(repo);
  const all = useMemo(
    () => flatItems(q.data).sort((a, b) => hitName(a).localeCompare(hitName(b))),
    [q.data],
  );
  const shown = filter ? all.filter((p) => hitName(p).toLowerCase().includes(filter)) : all;

  if (q.isError) return <ErrorNote what="packages" onRetry={() => void q.refetch()} />;
  if (q.isPending) return <p className="meta tree-list__empty">Loading packages…</p>;
  const total = q.data.pages[0]?.total ?? all.length;

  return (
    <>
      <p className="meta tree-count" aria-live="polite">
        {filter ? `${shown.length} of ${total} packages` : total === 1 ? '1 package' : `${total} packages`}
      </p>
      <ul className="tree-list" aria-label={`Packages in ${repo}`}>
        {shown.length === 0 && (
          <li className="meta tree-list__empty">{filter ? 'No package matches.' : 'Nothing published here yet.'}</li>
        )}
        {shown.map((p) => (
          <PackageRow key={p.component_id} hit={p} selected={name === p.name && (ns === undefined || sameNs(ns, p.namespace))} version={version} />
        ))}
      </ul>
      {q.hasNextPage && (
        <button type="button" className="button button--secondary button--sm" onClick={() => void q.fetchNextPage()} disabled={q.isFetchingNextPage}>
          {q.isFetchingNextPage ? 'Loading…' : 'Load more packages'}
        </button>
      )}
    </>
  );
}

function PackageRow({ hit, selected, version }: { hit: SearchHit; selected: boolean; version?: string }) {
  const label = hitName(hit);
  const base = { repo: hit.repository, ns: hit.namespace || undefined, name: hit.name };
  return (
    <li>
      <Link
        to="/browse"
        search={base}
        className={selected ? 'tree-item is-selected' : 'tree-item'}
        aria-current={selected && !version ? 'page' : undefined}
      >
        <span className="tree-item__dot" aria-hidden="true" />
        <span className="tree-item__name">{label}</span>
        <span className="tree-item__count" title={`${hit.versions} versions`}>
          {hit.versions}
          <span className="visually-hidden"> versions</span>
        </span>
      </Link>
      {selected && hasVersion(hit.version) && <VersionList hit={hit} version={version} />}
    </li>
  );
}

const TREE_VERSIONS = 8;

/** The newest versions of the selected package (shares the lookup the detail view makes). */
function VersionList({ hit, version }: { hit: SearchHit; version?: string }) {
  const lookup = useComponentLookup(hit.repository, hit.name);
  const ns = hit.namespace ?? '';
  const versions = (lookup.data?.items ?? []).filter((c) => (c.namespace ?? '') === ns);
  if (!versions.length) return null;
  const active = version ?? versions[0]?.version;
  const base = { repo: hit.repository, ns: hit.namespace || undefined, name: hit.name };
  return (
    <ul className="tree-versions" aria-label={`Versions of ${hit.name}`}>
      {versions.slice(0, TREE_VERSIONS).map((v, i) => (
        <li key={v.id}>
          <Link
            to="/browse"
            search={{ ...base, version: v.version }}
            className={v.version === active ? 'tree-version is-active' : 'tree-version'}
            aria-current={v.version === active ? 'page' : undefined}
          >
            <span className="tree-version__name">{v.version}</span>
            {i === 0 && <span className="tree-version__tag">newest</span>}
          </Link>
        </li>
      ))}
      {versions.length > TREE_VERSIONS && (
        <li>
          <Link to="/browse" search={{ ...base, version: active, tab: 'versions' }} className="tree-version tree-version--more">
            All {versions.length} versions
          </Link>
        </li>
      )}
    </ul>
  );
}
