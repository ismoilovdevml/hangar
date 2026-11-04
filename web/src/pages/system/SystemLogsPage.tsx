import { useEffect, useId, useRef, useState } from 'react';
import { getRouteApi } from '@tanstack/react-router';
import { ApiRequestError } from '../../api/client';
import { errorText, saveBlob, useCanSystem } from '../../api/ops';
import { ErrorNote } from '../../components/ErrorNote';
import { IconSearch } from '../../components/icons';
import { IconDownload } from '../../components/opsIcons';
import { PageHeader } from '../../components/PageHeader';
import { Panel } from '../../components/Panel';
import { fetchWholeBuffer, isOlderServer, LOGS_REFRESH_MS, useLogStream, useSystemNodes, type LogFilters } from './api';
import { AutoRefresh } from './AutoRefresh';
import { LogList } from './LogList';
import { isLogLevel, LOG_LEVELS, toJsonLines, type SystemLogsSearch } from './model';
import { Picker, type PickerOption } from './Picker';
import './system.css';

const route = getRouteApi('/app/system/logs');

/** Text filter: typed locally, written to the URL after a short pause. */
function TextFilter({ value, onChange }: { value: string; onChange: (q: string | undefined) => void }) {
  const [draft, setDraft] = useState(value);
  const [synced, setSynced] = useState(value);
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
    const t = setTimeout(() => commit.current(draft.trim() || undefined), 350);
    return () => clearTimeout(t);
  }, [draft, value]);
  const id = useId();
  return (
    <div className="filter-search log-filter">
      <label htmlFor={id} className="visually-hidden">
        Filter log records
      </label>
      <IconSearch size={16} />
      <input
        id={id}
        className="filter-search__input"
        type="search"
        placeholder="Filter text, e.g. nuget-proxy"
        value={draft}
        maxLength={200}
        onChange={(e) => setDraft(e.target.value)}
        autoComplete="off"
        spellCheck={false}
      />
    </div>
  );
}

function Forbidden() {
  return (
    <Panel>
      <p className="meta">
        System logs need the <code className="inline-code">system:cluster:admin</code> privilege: records name users, client addresses and internal
        hosts. Ask an administrator for it.
      </p>
    </Panel>
  );
}

interface LogsViewProps {
  filters: LogFilters;
  nodeOptions: PickerOption<string>[];
  onNode: (node: string) => void;
  onFilter: (p: SystemLogsSearch) => void;
}

function LogsView({ filters, nodeOptions, onNode, onFilter }: LogsViewProps) {
  const [paused, setPaused] = useState(false);
  const stream = useLogStream(filters, paused);
  const [download, setDownload] = useState<{ busy: boolean; error?: string }>({ busy: false });

  const forbidden = stream.error instanceof ApiRequestError && stream.error.status === 403;
  const node = stream.node ?? filters.node ?? '';
  const filtered = !!(filters.level || filters.q);

  const onDownload = async () => {
    setDownload({ busy: true });
    try {
      const records = await fetchWholeBuffer(filters);
      const stamp = new Date().toISOString().replace(/[:.]/g, '-').slice(0, 19);
      saveBlob(new Blob([toJsonLines(records)], { type: 'application/x-ndjson' }), `hangar-logs-${node || 'node'}-${stamp}.jsonl`);
      setDownload({ busy: false });
    } catch (err) {
      setDownload({ busy: false, error: `Download failed: ${errorText(err)}` });
    }
  };

  if (forbidden) return <Forbidden />;

  return (
    <>
      <div className="logs-refresh">
        <AutoRefresh
          what="logs"
          updatedAt={stream.updatedAt}
          intervalMs={LOGS_REFRESH_MS}
          paused={paused}
          fetching={stream.fetching}
          onPause={setPaused}
          onRefresh={stream.refresh}
        />
      </div>
      <Panel flush aria-label="Log records" className="logs-panel">
        <div className="repo-toolbar logs-toolbar">
          <div className="repo-toolbar__filters repo-toolbar__filters--start">
            <Picker label="Node" options={nodeOptions} value={filters.node ?? ''} onChange={onNode} />
            <Picker
              label="Level"
              options={LOG_LEVELS.map((l) => ({ value: l.value, label: l.label }))}
              value={filters.level ?? ''}
              onChange={(v) => onFilter({ level: isLogLevel(v) ? v : undefined })}
            />
            <TextFilter value={filters.q ?? ''} onChange={(q) => onFilter({ q })} />
          </div>
          <button type="button" className="button button--secondary" onClick={() => void onDownload()} disabled={download.busy || stream.status !== 'ready'}>
            <IconDownload size={16} />
            {download.busy ? 'Preparing…' : 'Download JSON lines'}
          </button>
        </div>
        {download.error && (
          <p className="field__error panel-note" role="alert">
            {download.error}
          </p>
        )}

        {stream.status === 'error' ? (
          isOlderServer(stream.error) ? (
            <p className="meta panel-note">This server does not keep logs in memory yet. Upgrade Hangar to read them here.</p>
          ) : (
            <div className="panel-note">
              <p className="meta">{errorText(stream.error, 'The logs could not be loaded.')}</p>
              <ErrorNote what="the log records" onRetry={stream.refresh} />
            </div>
          )
        ) : stream.status === 'loading' ? (
          <p className="meta panel-note" aria-busy="true">
            Loading log records…
          </p>
        ) : (
          <>
            <div className="logs-meta">
              <p className="meta" aria-live="polite">
                {stream.records.length === 0
                  ? filtered
                    ? 'No records match these filters.'
                    : 'No records in the buffer yet.'
                  : `Showing ${stream.records.length.toLocaleString('en-US')} record${stream.records.length === 1 ? '' : 's'}`}
                {stream.buffer && ` · node ${node} keeps ${stream.buffer.size.toLocaleString('en-US')} of ${stream.buffer.capacity.toLocaleString('en-US')}`}
              </p>
              {stream.error !== undefined && <p className="field__error">The last refresh failed: {errorText(stream.error)}</p>}
            </div>
            {stream.gap && (
              <p className="logs-gap" role="note">
                Some records were skipped: the buffer overwrote them before they were shown, or the node restarted.
              </p>
            )}
            {stream.records.length > 0 && <LogList records={stream.records} label={`Log records of ${node}`} />}
          </>
        )}
        <p className="meta panel-note logs-footnote">
          Only the newest records are kept, in memory, and secrets are removed before they are stored. Full logs are written to the container or
          journal: run <code className="inline-code">docker compose logs hangar</code> or <code className="inline-code">journalctl -u hangar</code> on
          the host.
        </p>
      </Panel>
    </>
  );
}

export function SystemLogsPage() {
  const search = route.useSearch();
  const navigate = route.useNavigate();
  const canAdmin = useCanSystem('cluster', 'admin');
  const nodes = useSystemNodes(false, canAdmin);

  const onFilter = (patch: SystemLogsSearch) => void navigate({ search: (s) => ({ ...s, ...patch }), replace: 'q' in patch });

  // Always name the node: behind a load balancer each request may reach a
  // different one, and a cursor only means something to the node that made it.
  const current = nodes.data?.current;
  const nodeOptions: PickerOption<string>[] = nodes.data
    ? nodes.data.items.map((n) => ({
        value: n.id,
        label: n.id,
        hint: n.current ? 'answering node' : n.stale ? 'not responding' : undefined,
      }))
    : [{ value: '', label: 'This node' }];
  if (search.node && !nodeOptions.some((o) => o.value === search.node)) nodeOptions.push({ value: search.node, label: search.node, hint: 'not listed' });
  const resolvedNode = search.node ?? current;
  const ready = !canAdmin || nodes.data !== undefined || nodes.isError;

  return (
    <div className="screen">
      <PageHeader
        title="System logs"
        description="Recent log records each node keeps in memory, newest at the bottom. Open a line to see all of its fields."
      />
      {!canAdmin ? (
        <Forbidden />
      ) : !ready ? (
        <Panel aria-busy="true">
          <p className="meta">Finding the nodes…</p>
        </Panel>
      ) : (
        <LogsView
          key={resolvedNode ?? ''}
          filters={{ node: resolvedNode, level: search.level, q: search.q }}
          nodeOptions={nodeOptions}
          onNode={(v) => onFilter({ node: v && v !== current ? v : undefined })}
          onFilter={onFilter}
        />
      )}
    </div>
  );
}
