import { useEffect, useId, useRef, useState, type ComponentType, type SVGProps } from 'react';
import { getRouteApi, Link, useRouterState } from '@tanstack/react-router';
import { useRepositories } from '../../api/hooks';
import type { Repository, RepositoryCounts, RepositoryType } from '../../api/types';
import { CopyButton } from '../../components/CopyButton';
import { DataTable, type Column, type TableSort } from '../../components/DataTable';
import { ErrorNote } from '../../components/ErrorNote';
import { IconDatabase, IconGlobe, IconGroup, IconPackage, IconPlus, IconSearch } from '../../components/icons';
import { PageHeader } from '../../components/PageHeader';
import { Panel } from '../../components/Panel';
import { FormatName } from '../../components/FormatIcon';
import { formatOptions } from '../../components/formatOptions';
import { Select } from '../../components/Select';
import { StatusDot } from '../../components/Pill';
import { formatBytes, relativeTime } from '../../lib/format';
import { plural } from '../../lib/overview';
import { formatUtc } from '../../lib/tasks';
import { useNow } from '../../lib/useNow';
import { ALL_FORMATS, displayUrl, isFormat, isRepoSort, repoSubtitle, STATUS_META, TYPE_META } from '../../lib/repositories';

const route = getRouteApi('/app/repositories');

export const PAGE_SIZE = 20;

const CARD_ICON: Record<RepositoryType, ComponentType<SVGProps<SVGSVGElement> & { size?: number }>> = {
  hosted: IconDatabase,
  proxy: IconGlobe,
  group: IconGroup,
};

function CountCards({ counts }: { counts: RepositoryCounts | undefined }) {
  return (
    <section className="count-cards" aria-label="Repository counts">
      {(['hosted', 'proxy', 'group'] as const).map((t) => {
        const Icon = CARD_ICON[t];
        const meta = TYPE_META[t];
        return (
          <Link key={t} to="/repositories" search={{ type: t }} className="count-card glass">
            <span className={`count-card__icon tone-${meta.tone}`} aria-hidden="true">
              <Icon size={20} />
            </span>
            <span className="count-card__text">
              <span className="count-card__number">{counts ? counts[t] : <span className="skeleton" style={{ width: 36, height: 28 }} />}</span>{' '}
              <span className="count-card__label">
                {meta.label} · {meta.card}
              </span>
            </span>
          </Link>
        );
      })}
    </section>
  );
}

const TABS: { id: RepositoryType | undefined; label: string; key: keyof RepositoryCounts }[] = [
  { id: undefined, label: 'All', key: 'all' },
  { id: 'hosted', label: 'Hosted', key: 'hosted' },
  { id: 'proxy', label: 'Proxy', key: 'proxy' },
  { id: 'group', label: 'Group', key: 'group' },
];

/** Name filter: typed locally, written to the URL after a short pause. */
function NameFilter({ value, onChange }: { value: string; onChange: (q: string | undefined) => void }) {
  const [draft, setDraft] = useState(value);
  const [synced, setSynced] = useState(value);
  // The URL changed from elsewhere (tab click, back button): adopt it.
  if (value !== synced) {
    setSynced(value);
    setDraft(value);
  }
  const commit = useRef(onChange);
  useEffect(() => {
    commit.current = onChange;
  }, [onChange]);
  useEffect(() => {
    if (draft.trim() === value) return;
    const t = setTimeout(() => commit.current(draft.trim() || undefined), 300);
    return () => clearTimeout(t);
  }, [draft, value]);
  const id = useId();
  return (
    <div className="filter-search">
      <label htmlFor={id} className="visually-hidden">
        Filter by name
      </label>
      <IconSearch size={16} />
      <input
        id={id}
        className="filter-search__input"
        type="search"
        placeholder="Filter by name"
        value={draft}
        onChange={(e) => setDraft(e.target.value)}
        autoComplete="off"
        spellCheck={false}
      />
    </div>
  );
}

/**
 * Stored size. A group stores nothing itself: it shows what it serves, its
 * members' content with shared blobs counted once, and how many versions.
 */
function RepoSize({ r }: { r: Repository }) {
  if (r.type !== 'group') return formatBytes(r.size_bytes);
  const members = r.config.group?.members.length ?? 0;
  return (
    <span className="repo-size" title="Served from the members; blobs they share are counted once">
      <span>
        {formatBytes(r.size_bytes)} <span className="repo-size__sub">via {plural(members, 'member')}</span>
      </span>
      <span className="repo-size__sub">{plural(r.component_count, 'version')}</span>
    </span>
  );
}

export function RepositoriesPage() {
  const search = route.useSearch();
  const navigate = route.useNavigate();
  const pageCursors = useRouterState({ select: (s) => s.location.state.pageCursors });
  const { type, format, q, sort, cursor } = search;
  const list = useRepositories({ type, format, q, sort, cursor, limit: PAGE_SIZE });
  const now = useNow(60_000);
  const order: TableSort = { key: (sort ?? 'name').replace(/^-/, ''), desc: !!sort?.startsWith('-') };
  const onSort = (key: string) => {
    // A new column starts ascending, except activity, which starts with the most recent.
    const desc = key === order.key ? !order.desc : key === 'last_activity';
    const next = `${desc ? '-' : ''}${key}`;
    setFilter({ sort: isRepoSort(next) && next !== 'name' ? next : undefined });
  };
  const data = list.data;
  const formatId = useId();

  const setFilter = (patch: Partial<typeof search>) =>
    void navigate({ search: (s) => ({ ...s, ...patch, cursor: undefined }), replace: 'q' in patch });

  const stack = cursor ? (pageCursors ?? []) : [];
  const goNext = () => {
    if (!data?.next_cursor) return;
    void navigate({
      search: (s) => ({ ...s, cursor: data.next_cursor ?? undefined }),
      state: { pageCursors: [...stack, cursor ?? ''] },
    });
  };
  const goPrev = () => {
    // Without the history of cursors (a shared link), "Previous" returns to the first page.
    const prev = stack.length ? stack[stack.length - 1] : '';
    void navigate({
      search: (s) => ({ ...s, cursor: prev || undefined }),
      state: { pageCursors: stack.slice(0, -1) },
    });
  };

  const filtered = !!(format || q);
  const tabTotal = data ? data.counts[type ?? 'all'] : 0;

  const columns: Column<Repository>[] = [
    {
      key: 'name',
      header: 'Name',
      sortable: true,
      className: 'col-first',
      cell: (r) => (
        <div className="repo-name">
          <Link to="/repositories/$name/settings" params={{ name: r.name }} className="repo-name__link">
            {r.name}
          </Link>
          {repoSubtitle(r) && <span className="repo-name__sub">{repoSubtitle(r)}</span>}
        </div>
      ),
    },
    {
      key: 'type',
      header: 'Type',
      sortable: true,
      cell: (r) => {
        const Icon = CARD_ICON[r.type];
        return (
          <span className={`pill pill--icon tone-${TYPE_META[r.type].tone}`}>
            <Icon size={13} aria-hidden="true" />
            {TYPE_META[r.type].label}
          </span>
        );
      },
    },
    { key: 'format', header: 'Format', sortable: true, className: 'muted', cell: (r) => <FormatName format={r.format} /> },
    {
      key: 'url',
      header: 'URL',
      cell: (r) => (
        <code className="repo-url" title={r.url}>
          {displayUrl(r.url)}
        </code>
      ),
    },
    {
      key: 'size',
      header: 'Size',
      align: 'right',
      className: 'muted nowrap',
      cell: (r) => <RepoSize r={r} />,
    },
    {
      key: 'last_activity',
      header: 'Last activity',
      sortable: true,
      className: 'meta nowrap',
      cell: (r) =>
        r.last_activity_at ? (
          <time dateTime={r.last_activity_at} title={formatUtc(r.last_activity_at)}>
            {relativeTime(r.last_activity_at, now)}
          </time>
        ) : (
          'No activity yet'
        ),
    },
    {
      key: 'status',
      header: 'Status',
      className: 'nowrap',
      cell: (r) => {
        const s = STATUS_META[r.status];
        return <StatusDot tone={s.tone} label={s.label} />;
      },
    },
    {
      key: 'copy',
      header: <span className="visually-hidden">Actions</span>,
      align: 'right',
      className: 'col-last',
      cell: (r) => (
        <span className="row-actions">
          <Link to="/browse" search={{ repo: r.name }} className="copy-button" aria-label={`Browse packages in ${r.name}`} title="Browse packages">
            <IconPackage size={16} />
          </Link>
          <CopyButton text={r.url} label={`Copy URL of ${r.name}`} />
        </span>
      ),
    },
  ];

  const empty = list.isPending ? (
    'Loading…'
  ) : filtered || type ? (
    <>
      No repositories match these filters.{' '}
      <Link to="/repositories" search={{}}>
        Show all
      </Link>
    </>
  ) : (
    <>
      No repositories yet. <Link to="/repositories/new">Create the first one</Link>.
    </>
  );

  return (
    <div className="screen">
      <PageHeader
        title="Repositories"
        description="Where packages live. Hosted stores your own, proxy caches the internet, group joins them under one URL."
        actions={
          <Link to="/repositories/new" search={type ? { type } : {}} className="button button--primary">
            <IconPlus size={16} /> New repository
          </Link>
        }
      />

      <CountCards counts={data?.counts} />

      <Panel flush className="repo-panel" aria-label="Repository list">
        <div className="repo-toolbar">
          <nav aria-label="Repository type" className="tabs">
            {TABS.map((t) => {
              const active = t.id === type;
              return (
                <Link
                  key={t.label}
                  to="/repositories"
                  search={{ type: t.id, format, q, sort }}
                  className={active ? 'tabs__item is-active' : 'tabs__item'}
                  aria-current={active ? 'page' : undefined}
                  activeOptions={{ includeSearch: false }}
                >
                  {t.label}{' '}
                  <span className="tabs__count">{data ? data.counts[t.key] : ''}</span>
                </Link>
              );
            })}
          </nav>
          <div className="repo-toolbar__filters">
            <div className="filter-select">
              <label htmlFor={formatId} className="filter-select__label">
                Format
              </label>
              <Select
                id={formatId}
                variant="inline"
                value={format ?? ''}
                options={formatOptions(ALL_FORMATS, 'All formats')}
                onChange={(v) => setFilter({ format: isFormat(v) ? v : undefined })}
              />
            </div>
            <NameFilter value={q ?? ''} onChange={(v) => setFilter({ q: v })} />
          </div>
        </div>

        {list.isError ? (
          <ErrorNote what="repositories" onRetry={() => void list.refetch()} />
        ) : (
          <DataTable
            caption="Repositories"
            columns={columns}
            rows={data?.items ?? []}
            rowKey={(r) => r.id}
            busy={list.isFetching}
            minWidth={980}
            empty={empty}
            sort={order}
            onSort={onSort}
          />
        )}

        <div className="repo-footer">
          <p className="meta" aria-live="polite">
            {data ? `Showing ${data.items.length} of ${tabTotal}` : ' '}
          </p>
          <div className="repo-footer__pager">
            <button type="button" className="button button--secondary button--sm" onClick={goPrev} disabled={!cursor}>
              Previous
            </button>
            <button type="button" className="button button--secondary button--sm" onClick={goNext} disabled={!data?.next_cursor}>
              Next
            </button>
          </div>
        </div>
      </Panel>
    </div>
  );
}
