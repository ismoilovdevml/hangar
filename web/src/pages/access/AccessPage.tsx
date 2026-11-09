import { useState } from 'react';
import { getRouteApi, Link } from '@tanstack/react-router';
import { useAuthProviders, useSession } from '../../api/hooks';
import { ErrorNote } from '../../components/ErrorNote';
import { IconPlus } from '../../components/icons';
import { PageHeader } from '../../components/PageHeader';
import { Panel } from '../../components/Panel';
import { StatusDot } from '../../components/Pill';
import { Segmented } from '../../components/Segmented';
import { useRoles, useTokens } from './api';
import { AuditTab, RecentAccessActivity } from './audit';
import { MemberDialog, MembersTab, UserDrawer } from './members';
import { allows, type AccessSearch, type AccessTab, type MemberFilters } from './model';
import { RolesTab } from './roles';
import { CreateTokenDialog, TokenTable } from './tokens';
import './access.css';

const route = getRouteApi('/app/access');

export function AccessPage() {
  const search = route.useSearch();
  const navigate = route.useNavigate();
  const session = useSession();
  const privs = session.data?.privileges;
  const can = {
    readUsers: allows(privs, 'system', 'users', 'read'),
    writeUsers: allows(privs, 'system', 'users', 'write'),
    deleteUsers: allows(privs, 'system', 'users', 'delete'),
    readTokens: allows(privs, 'system', 'tokens', 'read'),
    writeTokens: allows(privs, 'system', 'tokens', 'write'),
    readAudit: allows(privs, 'system', 'audit', 'read'),
  };
  const roles = useRoles(can.readUsers);
  const [creating, setCreating] = useState<'user' | 'service_account' | 'token' | null>(null);

  const tabs: { id: AccessTab; label: string; count?: number; visible: boolean }[] = [
    { id: 'members', label: 'Members', visible: can.readUsers },
    { id: 'service-accounts', label: 'Service accounts', visible: can.readUsers },
    { id: 'tokens', label: 'Access tokens', visible: can.readTokens },
    { id: 'roles', label: 'Roles', count: roles.data?.items.length, visible: can.readUsers },
    { id: 'audit', label: 'Audit log', visible: can.readAudit },
  ];
  const visible = tabs.filter((t) => t.visible);
  const tab: AccessTab | undefined = visible.find((t) => t.id === search.tab)?.id ?? visible[0]?.id;

  const set = (patch: Partial<AccessSearch>, replace = false) => void navigate({ search: (s) => ({ ...s, ...patch }), replace });
  const filters: MemberFilters = { q: search.q, role: search.role, status: search.status };

  if (!session.data) return null;
  if (!tab) {
    return (
      <div className="screen">
        <PageHeader title="Access & audit" description="Who can read, publish and administer — and a record of everything they did." />
        <Panel>
          <p>
            Managing people and roles needs an administrator role. Your own profile, password, tokens and sessions are under{' '}
            <Link to="/account">My account</Link>.
          </p>
        </Panel>
      </div>
    );
  }

  const action =
    tab === 'members' && can.writeUsers ? (
      <button type="button" className="button button--primary" onClick={() => setCreating('user')}>
        <IconPlus size={16} /> Add member
      </button>
    ) : tab === 'service-accounts' && can.writeUsers ? (
      <button type="button" className="button button--primary" onClick={() => setCreating('service_account')}>
        <IconPlus size={16} /> New service account
      </button>
    ) : tab === 'tokens' ? (
      <button type="button" className="button button--primary" onClick={() => setCreating('token')}>
        <IconPlus size={16} /> New token
      </button>
    ) : null;

  return (
    <div className="screen">
      <PageHeader title="Access & audit" description="Who can read, publish and administer — and a record of everything they did." actions={action} />

      <nav aria-label="Access sections" className="tabs tabs--glass glass">
        {visible.map((t) => {
          const active = t.id === tab;
          return (
            <Link
              key={t.id}
              to="/access"
              search={{ tab: t.id }}
              className={active ? 'tabs__item is-active' : 'tabs__item'}
              aria-current={active ? 'page' : undefined}
              activeOptions={{ includeSearch: false }}
            >
              {t.label}
              {t.count !== undefined && <span className="tabs__count">{t.count}</span>}
            </Link>
          );
        })}
      </nav>

      {tab === 'members' ? (
        <div className="access-layout">
          <MembersTab kind="user" filters={filters} onFilters={(f) => set(f, true)} onOpen={(user) => set({ user })} />
          <div className="access-layout__side">
            <ProvidersPanel />
            {can.readAudit && <RecentAccessActivity onShowAll={() => set({ tab: 'audit' })} />}
          </div>
        </div>
      ) : tab === 'service-accounts' ? (
        <>
          <p className="meta access-hint">
            Service accounts are for CI systems and automation. They have no password: create a token for each pipeline in the account’s drawer, with
            only the repositories and actions it needs.
          </p>
          <MembersTab kind="service_account" filters={filters} onFilters={(f) => set(f, true)} onOpen={(user) => set({ user })} />
        </>
      ) : tab === 'tokens' ? (
        <AllTokens ownerKind={search.owner_kind} status={search.token_status} q={search.q} onChange={(p) => set(p, true)} />
      ) : tab === 'roles' ? (
        <RolesTab canWrite={can.writeUsers} />
      ) : (
        <AuditTab category={search.events ?? 'users'} onCategory={(events) => set({ events }, true)} />
      )}

      {search.user && can.readUsers && (
        <UserDrawer
          id={search.user}
          canWrite={can.writeUsers}
          canDelete={can.deleteUsers}
          canManageTokens={can.writeTokens}
          onClose={() => set({ user: undefined })}
        />
      )}
      {(creating === 'user' || creating === 'service_account') && (
        <MemberDialog kind={creating} onClose={() => setCreating(null)} onCreated={(m) => set({ user: m.id })} />
      )}
      {creating === 'token' && <CreateTokenDialog onClose={() => setCreating(null)} />}
    </div>
  );
}

function ProvidersPanel() {
  const providers = useAuthProviders();
  const p = providers.data;
  const rows = p
    ? [
        { name: p.oidc ? `${p.oidc.display_name} (OIDC)` : 'Single sign-on (OIDC)', on: !!p.oidc },
        { name: 'Directory (LDAP)', on: p.ldap.enabled },
        { name: 'SAML', on: p.saml.enabled },
        { name: 'Local accounts', on: p.local.enabled },
      ]
    : [];
  return (
    <Panel className="side-panel side-panel--tight" title="Sign-in providers" titleId="providers-title">
      {providers.isError ? (
        <p className="meta">Could not load sign-in providers.</p>
      ) : (
        <ul className="provider-list">
          {rows.map((r) => (
            <li key={r.name} className="provider-list__row">
              <StatusDot tone={r.on ? 'ok' : 'neutral'} label={<span className="cell-strong">{r.name}</span>} />
              <span className="meta">{r.on ? 'Enabled' : 'Not configured'}</span>
            </li>
          ))}
        </ul>
      )}
    </Panel>
  );
}

interface AllTokensProps {
  ownerKind?: 'user' | 'service_account';
  status?: 'active' | 'expired';
  q?: string;
  onChange: (p: Partial<AccessSearch>) => void;
}

function AllTokens({ ownerKind, status, q, onChange }: AllTokensProps) {
  const [cursors, setCursors] = useState<string[]>([]);
  const cursor = cursors[cursors.length - 1];
  const tokens = useTokens({ all: true, owner_kind: ownerKind, status, q, limit: 50, cursor });
  const reset = (p: Partial<AccessSearch>) => {
    setCursors([]);
    onChange(p);
  };
  return (
    <Panel flush aria-label="Access tokens">
      <div className="repo-toolbar">
        <Segmented
          label="Owner"
          value={ownerKind ?? 'all'}
          options={[
            { value: 'all', label: 'Everyone' },
            { value: 'user', label: 'People' },
            { value: 'service_account', label: 'Service accounts' },
          ]}
          onChange={(v) => reset({ owner_kind: v === 'all' ? undefined : (v as 'user' | 'service_account') })}
        />
        <Segmented
          label="Status"
          value={status ?? 'any'}
          options={[
            { value: 'any', label: 'Any' },
            { value: 'active', label: 'Active' },
            { value: 'expired', label: 'Expired' },
          ]}
          onChange={(v) => reset({ token_status: v === 'any' ? undefined : (v as 'active' | 'expired') })}
        />
      </div>
      {tokens.isError ? (
        <ErrorNote what="tokens" error={tokens.error} privilege="system:tokens:read" onRetry={() => void tokens.refetch()} />
      ) : (
        <TokenTable caption="All access tokens" tokens={tokens.data?.items ?? []} showOwner busy={tokens.isFetching} empty={tokens.isPending ? 'Loading…' : 'No tokens match.'} />
      )}
      <div className="repo-footer">
        <p className="meta">Revoked tokens are not listed; their history is in the audit log.</p>
        <div className="repo-footer__pager">
          <button type="button" className="button button--secondary button--sm" disabled={!cursors.length} onClick={() => setCursors((c) => c.slice(0, -1))}>
            Previous
          </button>
          <button
            type="button"
            className="button button--secondary button--sm"
            disabled={!tokens.data?.next_cursor}
            onClick={() => tokens.data?.next_cursor && setCursors((c) => [...c, tokens.data!.next_cursor!])}
          >
            Next
          </button>
        </div>
      </div>
    </Panel>
  );
}
