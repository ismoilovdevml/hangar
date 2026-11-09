import { Link, useRouter } from '@tanstack/react-router';
import { useAuthProviders, useBlobStores, useSession, useSystemStatus } from '../../api/hooks';
import { IconChevronRight } from '../../components/opsIcons';
import { canSee, visibleHub, type AppPath, type HubEntry, type HubEntryId } from '../../nav';
import { formatBytes, instanceName } from '../../lib/format';
import { formatUtc } from '../../lib/tasks';
import { useSystemNodes } from '../system/api';
import { summarize, versionText } from '../system/model';
import { useIdentityProviders, useProxies } from './settingsApi';
import './hub.css';

const STATE_TONE = { ok: 'ok', degraded: 'warn', down: 'bad' } as const;

/** A row's status line: text (monospace for addresses), or still loading, or not readable. */
type Line = { text: string; mono?: boolean; attention?: boolean } | 'pending' | 'unavailable';

interface QueryLike<T> {
  data?: T;
  isError: boolean;
}

/** `pending` while the query loads, `unavailable` when it failed, else what `fn` makes of the data. */
function lineFrom<T>(q: QueryLike<T>, fn: (data: T) => Line): Line {
  if (q.data !== undefined) return fn(q.data);
  return q.isError ? 'unavailable' : 'pending';
}

const plural = (n: number, one: string, many = `${one}s`) => `${n} ${n === 1 ? one : many}`;

/**
 * Settings: one place for everything about this installation and the signed-in
 * user. Each row leads to the page that changes it and says, in a few words,
 * how it is set now. Rows the user may not see are left out.
 */
export function SettingsHubPage() {
  const router = useRouter();
  const session = useSession();
  const privileges = session.data?.privileges;
  const can = (area: string, action: 'read' | 'admin') => canSee(privileges, { area, action });
  const canSettings = can('settings', 'read');
  const canCluster = can('cluster', 'read');

  const status = useSystemStatus();
  const sys = status.data;
  const providers = useAuthProviders();
  const idps = useIdentityProviders(canSettings);
  const proxies = useProxies(canSettings);
  const stores = useBlobStores(can('blobstores', 'read'));
  const nodes = useSystemNodes(true, canCluster);

  // The API reference ships with newer builds; link to it only when this build has the page.
  const hasApiDocs = '/api-docs' in (router.routesByPath as Record<string, unknown>);
  const groups = visibleHub(privileges)
    .map((g) => ({ ...g, entries: g.entries.filter((e) => e.to !== '/api-docs' || hasApiDocs) }))
    .filter((g) => g.entries.length > 0);

  const user = session.data?.user;
  const tokenWarnings = session.data?.token_warnings?.length ?? 0;
  const summary = nodes.data ? summarize(nodes.data.items) : undefined;

  const lines: Record<HubEntryId, () => Line> = {
    addresses: () => {
      if (sys?.base_url) return { text: sys.base_url, mono: true };
      if (!sys) return status.isError ? 'unavailable' : 'pending';
      return { text: 'Not set: clients see the address they used', attention: true };
    },
    instance: () => lineFrom(status, (s) => ({ text: s.instance_name || instanceName(session.data?.organization.name) })),
    'sign-in': () =>
      lineFrom(providers, (p) => {
        const ways = [
          p.oidc?.display_name || (p.oidc ? 'Single sign-on' : ''),
          p.saml.enabled ? 'SAML' : '',
          p.ldap.enabled ? 'LDAP' : '',
          p.local.enabled ? 'local accounts' : '',
        ].filter(Boolean);
        const failing = (idps.data?.items ?? []).filter((i) => i.enabled && i.state === 'error').length;
        if (failing) ways.push(`${failing} failing`);
        return ways.length ? { text: ways.join(' · '), attention: failing > 0 } : { text: 'No sign-in method is on', attention: true };
      }),
    internet: () =>
      lineFrom(proxies, (list) => {
        const chosen = list.items.find((p) => p.name === list.default) ?? list.items.find((p) => p.default);
        if (chosen) {
          const others = list.items.length - 1;
          return { text: [`${chosen.name} · ${chosen.host}:${chosen.port}`, others > 0 ? `${plural(others, 'more proxy', 'more proxies')}` : ''].filter(Boolean).join(' · ') };
        }
        if (list.environment_proxy) return { text: `${list.environment_proxy} from the server environment` };
        return { text: list.items.length ? `Direct · ${plural(list.items.length, 'proxy', 'proxies')} for some repositories` : 'Direct' };
      }),
    storage: () =>
      lineFrom(stores, ({ items, totals }) => {
        if (items.length === 0) return { text: 'No blob store yet', attention: true };
        const capacity = items.length === 1 ? (items[0]!.capacity_bytes ?? items[0]!.soft_quota_bytes ?? null) : (totals.capacity_bytes ?? null);
        const used = totals.used_bytes;
        const fill = capacity ? `${Math.max(0, Math.round((1 - used / capacity) * 100))}% free` : `${formatBytes(used)} used`;
        const unhealthy = items.filter((s) => s.state !== 'ok').length;
        const head = items.length === 1 ? `${items[0]!.name} · ${items[0]!.kind === 's3' ? 'S3' : 'filesystem'}` : plural(items.length, 'blob store');
        return { text: [head, fill, unhealthy ? `${unhealthy} not healthy` : ''].filter(Boolean).join(' · '), attention: unhealthy > 0 };
      }),
    nodes: () => {
      if (summary && summary.total > 0) {
        const leader = summary.leader ? `${summary.leader.hostname || summary.leader.id} leader` : 'no leader';
        return { text: `${summary.up} of ${summary.total} up · ${leader}`, attention: summary.up < summary.total || !summary.leader };
      }
      return lineFrom(status, (s) => ({ text: `${s.nodes_up} of ${s.nodes_total} up`, attention: s.nodes_up < s.nodes_total }));
    },
    configuration: () => ({ text: 'hangar.yaml + HANGAR_* environment' }),
    status: () => {
      if (summary && summary.checks > 0) {
        return summary.passing === summary.checks
          ? { text: `All ${plural(summary.checks, 'check')} passing` }
          : { text: `${summary.headline} · ${summary.passing} of ${summary.checks} passing`, attention: true };
      }
      return lineFrom(status, (s) => ({ text: s.message, attention: s.state !== 'ok' }));
    },
    logs: () => ({ text: 'Live server output · filter by node and level' }),
    audit: () => ({ text: 'Who changed what, and when' }),
    account: () => (user ? { text: [user.display_name, user.email || user.username].filter(Boolean).join(' · ') } : 'pending'),
    tokens: () =>
      tokenWarnings > 0
        ? { text: `${plural(tokenWarnings, 'token')} expiring soon`, attention: true }
        : { text: 'Personal tokens for CLIs and CI' },
    'api-docs': () => ({ text: 'Every endpoint, for scripts and tools' }),
  };

  return (
    <div className="screen hub-screen">
      <header className="hub-header">
        <h1 className="hub-header__title">Settings</h1>
        <p className="hub-header__desc">Everything about this Hangar in one place.</p>
      </header>

      {groups.map((g) => (
        <section key={g.label} className="hub-group glass" aria-label={g.label}>
          <ul className="hub-list">
            {g.entries.map((e) => (
              <li key={e.id}>
                <HubRow entry={e} line={lines[e.id]()} />
              </li>
            ))}
          </ul>
        </section>
      ))}

      {sys && (
        <p className="hub-foot">
          Hangar {versionText(sys.version)}
          {sys.build?.commit && <> · {sys.build.commit.slice(0, 12)}</>}
          {sys.build?.date && <> · built {formatUtc(sys.build.date).split(',')[0]}</>}
          {' · '}
          <Link to="/system/status" className={`hub-foot__status hub-foot__status--${STATE_TONE[sys.state]}`}>
            <span className={`status__dot dot-${STATE_TONE[sys.state]}`} aria-hidden="true" />
            {sys.message}
          </Link>
        </p>
      )}
    </div>
  );
}

function HubRow({ entry, line }: { entry: HubEntry; line: Line }) {
  return (
    <Link to={entry.to as AppPath} hash={entry.hash} className="hub-row">
      <span className="hub-row__key">{entry.label}</span>
      {line === 'pending' ? (
        <span className="hub-row__value" aria-busy="true">
          <span className="skeleton hub-row__skeleton" />
          <span className="visually-hidden">Loading</span>
        </span>
      ) : line === 'unavailable' ? (
        <span className="hub-row__value">Could not be read</span>
      ) : (
        <span className={['hub-row__value', line.mono ? 'hub-row__value--mono' : '', line.attention ? 'hub-row__value--attention' : ''].filter(Boolean).join(' ')}>
          {line.attention && <span className="status__dot dot-warn" aria-hidden="true" />}
          <span className="hub-row__text">{line.text}</span>
        </span>
      )}
      <IconChevronRight size={16} className="hub-row__chevron" />
    </Link>
  );
}
