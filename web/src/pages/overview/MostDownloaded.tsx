import { useQuery } from '@tanstack/react-query';
import { Link } from '@tanstack/react-router';
import { apiFetch } from '../../api/client';
import { catalogKeys } from '../../api/catalog';
import type { SearchResult } from '../../api/types';
import { ErrorNote } from '../../components/ErrorNote';
import { formatCompact } from '../../lib/format';
import { hitName } from '../../lib/packages';

const TOP = 6;

/** The packages pulled most often, all time, across the repositories the user can read. */
export function MostDownloaded() {
  const q = useQuery({
    queryKey: [...catalogKeys.searchQuery('', '-downloads'), 'top'],
    queryFn: ({ signal }) => apiFetch<SearchResult>('/search', { query: { sort: '-downloads', limit: TOP }, signal }),
    staleTime: 5 * 60_000,
  });
  const items = q.data?.items.filter((h) => h.downloads > 0) ?? [];
  // Names in the format's idiom, with the npm scope or Maven group: "@acme/ui-kit", "org.apache:commons-io".
  // The same package can sit in several repositories (a proxy and a group, say): name the repository then.
  const seen = new Map<string, number>();
  for (const h of items) seen.set(hitName(h), (seen.get(hitName(h)) ?? 0) + 1);

  return (
    <section className="ov-card top glass" aria-labelledby="top-title">
      <h2 className="top__title" id="top-title">
        Most downloaded
      </h2>
      {q.isError ? (
        <ErrorNote what="the most downloaded packages" error={q.error} onRetry={() => void q.refetch()} />
      ) : q.isPending ? (
        <span className="top__loading" aria-busy="true">
          {[140, 90, 120, 100].map((w) => (
            <span key={w} className="skeleton" style={{ width: w, height: 30 }} />
          ))}
        </span>
      ) : items.length === 0 ? (
        <p className="meta">No downloads yet. Packages show up here once clients pull them.</p>
      ) : (
        <ul className="top__list">
          {items.map((h) => (
            <li key={h.component_id}>
              <Link
                to="/browse"
                search={{ repo: h.repository, ns: h.namespace || undefined, name: h.name, version: h.version || undefined }}
                className="top__chip"
                title={`${h.coordinates} in ${h.repository}`}
              >
                <span className="top__name">{hitName(h)}</span>
                {(seen.get(hitName(h)) ?? 0) > 1 && (
                  <span className="top__repo">
                    <span className="visually-hidden"> in </span>
                    {h.repository}
                  </span>
                )}
                <span className="top__count">
                  {formatCompact(h.downloads)}
                  <span className="visually-hidden"> downloads</span>
                </span>
              </Link>
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}
