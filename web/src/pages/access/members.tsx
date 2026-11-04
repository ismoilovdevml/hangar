import { useEffect, useId, useRef, useState, type FormEvent } from 'react';
import { useSession } from '../../api/hooks';
import { DataTable, type Column } from '../../components/DataTable';
import { ErrorNote } from '../../components/ErrorNote';
import { IconSearch } from '../../components/icons';
import { Panel } from '../../components/Panel';
import { Pill, StatusDot } from '../../components/Pill';
import { relativeTime } from '../../lib/format';
import { useNow } from '../../lib/useNow';
import {
  useCreateMember,
  useDeleteMember,
  useEndUserSession,
  useEndUserSessions,
  useMember,
  useMembers,
  usePermissions,
  useResetPassword,
  useRoles,
  useSetDisabled,
  useTokens,
  useUnlock,
  useUpdateMember,
  useUserSessions,
  type Member,
  type MemberInput,
  type SessionInfo,
} from './api';
import {
  ACTIONS,
  describePrivilege,
  describeUserAgent,
  formatDate,
  generatePassword,
  roleTone,
  type MemberFilters,
  SOURCE_LABEL,
  SYSTEM_AREAS,
  USER_STATUS_META,
  USERNAME_RE,
  userStatus,
  type UserStatus,
} from './model';
import { CreateTokenDialog, TokenTable } from './tokens';
import { Avatar, ConfirmDialog, Dialog, Field, FormError, Notice } from './ui';

export const MEMBER_PAGE_SIZE = 50;

/* ---------------------------------------------------------------- list */

interface MembersTabProps {
  kind: 'user' | 'service_account';
  filters: MemberFilters;
  onFilters: (f: MemberFilters) => void;
  onOpen: (id: string) => void;
}

export function MembersTab({ kind, filters, onFilters, onOpen }: MembersTabProps) {
  const now = useNow();
  const [cursors, setCursors] = useState<string[]>([]);
  const cursor = cursors[cursors.length - 1];
  const list = useMembers({ kind, q: filters.q, role: filters.role, status: filters.status, limit: MEMBER_PAGE_SIZE, cursor });
  const roles = useRoles();
  const roleId = useId();
  const statusId = useId();
  const sa = kind === 'service_account';
  const filterKey = `${filters.q}|${filters.role}|${filters.status}|${kind}`;
  // New filters start again from the first page.
  const [lastKey, setLastKey] = useState(filterKey);
  if (lastKey !== filterKey) {
    setLastKey(filterKey);
    setCursors([]);
  }

  const columns: Column<Member>[] = [
    {
      key: 'member',
      header: sa ? 'Service account' : 'Member',
      className: 'col-first',
      cell: (m) => (
        <button type="button" className="member-cell" onClick={() => onOpen(m.id)} aria-label={`Open ${m.display_name || m.username}`}>
          <Avatar name={m.display_name || m.username} />
          <span className="cell-stack">
            <span className="cell-strong">{sa ? m.username : m.display_name || m.username}</span>
            <span className="meta">{sa ? m.description || 'service account' : (m.email ?? m.username)}</span>
          </span>
        </button>
      ),
    },
    {
      key: 'roles',
      header: 'Role',
      cell: (m) =>
        m.roles.length ? (
          <span className="pill-row">
            {m.roles.map((r) => (
              <Pill key={r} tone={roleTone(r)}>
                {r}
              </Pill>
            ))}
          </span>
        ) : (
          <span className="meta">No role</span>
        ),
    },
    { key: 'source', header: 'Source', className: 'muted', cell: (m) => (sa ? 'Token' : (SOURCE_LABEL[m.source] ?? m.source)) },
    {
      key: 'tokens',
      header: 'Tokens',
      className: 'muted nowrap',
      cell: (m) => (m.token_count ? `${m.token_count} active` : '—'),
    },
    {
      key: 'active',
      header: 'Last active',
      className: 'muted nowrap',
      cell: (m) => (m.last_active_at ? relativeTime(m.last_active_at, now) : 'Never'),
    },
    {
      key: 'status',
      header: 'Status',
      className: 'col-last nowrap',
      cell: (m) => {
        const st = USER_STATUS_META[userStatus(m, now)];
        return <StatusDot tone={st.tone} label={st.label} />;
      },
    },
  ];

  return (
    <Panel flush aria-label={sa ? 'Service accounts' : 'Members'}>
      <div className="repo-toolbar">
        <SearchBox value={filters.q ?? ''} label={sa ? 'Search service accounts' : 'Search members'} onChange={(q) => onFilters({ ...filters, q })} />
        <div className="repo-toolbar__filters">
          <div className="filter-select">
            <label htmlFor={roleId} className="filter-select__label">
              Role
            </label>
            <select id={roleId} className="filter-select__input" value={filters.role ?? ''} onChange={(e) => onFilters({ ...filters, role: e.target.value || undefined })}>
              <option value="">Any</option>
              {(roles.data?.items ?? []).map((r) => (
                <option key={r.id} value={r.name}>
                  {r.name}
                </option>
              ))}
            </select>
          </div>
          <div className="filter-select">
            <label htmlFor={statusId} className="filter-select__label">
              Status
            </label>
            <select
              id={statusId}
              className="filter-select__input"
              value={filters.status ?? ''}
              onChange={(e) => onFilters({ ...filters, status: (e.target.value || undefined) as UserStatus | undefined })}
            >
              <option value="">Any</option>
              <option value="active">Active</option>
              <option value="locked">Locked</option>
              <option value="disabled">Disabled</option>
            </select>
          </div>
        </div>
      </div>
      {list.isError ? (
        <ErrorNote what={sa ? 'service accounts' : 'members'} onRetry={() => void list.refetch()} />
      ) : (
        <DataTable
          caption={sa ? 'Service accounts' : 'Members'}
          columns={columns}
          rows={list.data?.items ?? []}
          rowKey={(m) => m.id}
          busy={list.isFetching}
          minWidth={820}
          empty={list.isPending ? 'Loading…' : filters.q || filters.role || filters.status ? 'Nobody matches these filters.' : sa ? 'No service accounts yet.' : 'No members yet.'}
        />
      )}
      <div className="repo-footer">
        <p className="meta" aria-live="polite">
          {list.data ? `${list.data.items.length} shown` : ' '}
        </p>
        <div className="repo-footer__pager">
          <button type="button" className="button button--secondary button--sm" disabled={!cursors.length} onClick={() => setCursors((c) => c.slice(0, -1))}>
            Previous
          </button>
          <button
            type="button"
            className="button button--secondary button--sm"
            disabled={!list.data?.next_cursor}
            onClick={() => list.data?.next_cursor && setCursors((c) => [...c, list.data!.next_cursor!])}
          >
            Next
          </button>
        </div>
      </div>
    </Panel>
  );
}

/** Search input that commits after a short pause. */
export function SearchBox({ value, label, onChange }: { value: string; label: string; onChange: (v: string | undefined) => void }) {
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
    const t = setTimeout(() => commit.current(draft.trim() || undefined), 300);
    return () => clearTimeout(t);
  }, [draft, value]);
  const id = useId();
  return (
    <div className="filter-search">
      <label htmlFor={id} className="visually-hidden">
        {label}
      </label>
      <IconSearch size={16} />
      <input id={id} className="filter-search__input" type="search" placeholder={label} value={draft} onChange={(e) => setDraft(e.target.value)} autoComplete="off" spellCheck={false} />
    </div>
  );
}

/* ---------------------------------------------------------------- create / edit */

function RolePicker({ value, onChange }: { value: string[]; onChange: (v: string[]) => void }) {
  const roles = useRoles();
  return (
    <fieldset className="fieldset">
      <legend className="field__label">Roles</legend>
      {roles.isPending && <p className="meta">Loading roles…</p>}
      <div className="option-grid">
        {(roles.data?.items ?? []).map((r) => (
          <label key={r.id} className={value.includes(r.name) ? 'option is-on' : 'option'}>
            <input
              type="checkbox"
              checked={value.includes(r.name)}
              onChange={() => onChange(value.includes(r.name) ? value.filter((x) => x !== r.name) : [...value, r.name])}
            />
            <span>
              <span className="option__title">{r.name}</span>
              <span className="option__hint">{r.description || r.privileges.map(describePrivilege).join('; ') || 'No privileges'}</span>
            </span>
          </label>
        ))}
      </div>
    </fieldset>
  );
}

interface MemberDialogProps {
  kind: 'user' | 'service_account';
  member?: Member;
  onClose: () => void;
  onCreated?: (m: Member) => void;
}

/** Create a member or service account, or edit one (name, email, description, roles). */
export function MemberDialog({ kind, member, onClose, onCreated }: MemberDialogProps) {
  const sa = kind === 'service_account';
  const [username, setUsername] = useState(member?.username ?? '');
  const [displayName, setDisplayName] = useState(member?.display_name ?? '');
  const [email, setEmail] = useState(member?.email ?? '');
  const [description, setDescription] = useState(member?.description ?? '');
  const [password, setPassword] = useState('');
  const [roles, setRoles] = useState<string[]>(member?.roles ?? (sa ? ['CI deployer'] : ['Reader']));
  const [submitted, setSubmitted] = useState(false);
  const create = useCreateMember();
  const update = useUpdateMember();
  const save = member ? update : create;
  const ids = { user: useId(), name: useId(), email: useId(), desc: useId(), pw: useId() };
  const external = member && member.source !== 'local';

  const errors = {
    username: !USERNAME_RE.test(username) ? 'Start with a letter or digit; letters, digits and . _ @ + - only.' : null,
    email: email && !/^[^\s@]+@[^\s@]+$/.test(email) ? 'Enter a valid email address.' : null,
    password: !member && !sa && password.length < 12 ? 'At least 12 characters (the server may require more).' : null,
  };

  const submit = (e: FormEvent) => {
    e.preventDefault();
    setSubmitted(true);
    if (errors.username || errors.email || errors.password) return;
    const body: MemberInput = { username, kind, roles, display_name: displayName, email };
    if (sa) body.description = description;
    if (member) update.mutate({ id: member.id, body }, { onSuccess: onClose });
    else {
      if (!sa) body.password = password;
      create.mutate(body, {
        onSuccess: (m) => {
          onCreated?.(m);
          onClose();
        },
      });
    }
  };

  return (
    <Dialog
      title={member ? `Edit ${member.display_name || member.username}` : sa ? 'New service account' : 'Add member'}
      description={
        sa
          ? 'Service accounts have no password. CI systems use their tokens, which an administrator creates.'
          : member
            ? undefined
            : 'Creates a local account with a password. People from single sign-on appear here after their first sign-in.'
      }
      onClose={onClose}
    >
      <form className="dialog-form" onSubmit={submit} noValidate>
        <div className="form-grid">
          <Field id={ids.user} label={sa ? 'Name' : 'Username'} error={submitted && errors.username}>
            <input className="input" value={username} onChange={(e) => setUsername(e.target.value)} autoComplete="off" spellCheck={false} readOnly={!!external} data-autofocus />
          </Field>
          <Field id={ids.name} label={<>Display name <span className="field__optional">(optional)</span></>}>
            <input className="input" value={displayName} onChange={(e) => setDisplayName(e.target.value)} maxLength={200} readOnly={!!external} />
          </Field>
          {!sa && (
            <Field id={ids.email} label={<>Email <span className="field__optional">(optional)</span></>} error={submitted && errors.email}>
              <input type="email" className="input" value={email} onChange={(e) => setEmail(e.target.value)} readOnly={!!external} />
            </Field>
          )}
          {sa && (
            <Field id={ids.desc} wide label={<>What uses it <span className="field__optional">(optional)</span></>}>
              <input className="input" value={description} onChange={(e) => setDescription(e.target.value)} placeholder="e.g. GitLab runners, group infra" maxLength={1000} />
            </Field>
          )}
          {!member && !sa && (
            <Field
              id={ids.pw}
              label="Initial password"
              error={submitted && errors.password}
              hint="Share it privately; they can change it under My account."
              addon={
                <button type="button" className="button button--secondary button--sm" onClick={() => setPassword(generatePassword())}>
                  Generate
                </button>
              }
            >
              <input className="input input--mono" type="text" value={password} onChange={(e) => setPassword(e.target.value)} autoComplete="new-password" />
            </Field>
          )}
        </div>
        <RolePicker value={roles} onChange={setRoles} />
        <FormError error={save.error} />
        <div className="dialog-actions">
          <button type="button" className="button button--secondary" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="button button--primary" disabled={save.isPending}>
            {save.isPending ? 'Saving…' : member ? 'Save changes' : sa ? 'Create service account' : 'Add member'}
          </button>
        </div>
      </form>
    </Dialog>
  );
}

function ResetPasswordDialog({ member, onClose }: { member: Member; onClose: () => void }) {
  const [password, setPassword] = useState('');
  const [done, setDone] = useState(false);
  const reset = useResetPassword();
  const id = useId();
  return (
    <Dialog title={`Reset password for ${member.display_name || member.username}`} description="Sets a new password, clears any lockout and signs them out everywhere." onClose={onClose}>
      {done ? (
        <div className="dialog-form">
          <Notice message="Password set. Share it with them privately." />
          <div className="dialog-actions">
            <button type="button" className="button button--primary" onClick={onClose}>
              Done
            </button>
          </div>
        </div>
      ) : (
        <form
          className="dialog-form"
          onSubmit={(e) => {
            e.preventDefault();
            reset.mutate({ id: member.id, password }, { onSuccess: () => setDone(true) });
          }}
        >
          <Field
            id={id}
            label="New password"
            addon={
              <button type="button" className="button button--secondary button--sm" onClick={() => setPassword(generatePassword())}>
                Generate
              </button>
            }
          >
            <input className="input input--mono" value={password} onChange={(e) => setPassword(e.target.value)} autoComplete="new-password" data-autofocus />
          </Field>
          <FormError error={reset.error} />
          <div className="dialog-actions">
            <button type="button" className="button button--secondary" onClick={onClose}>
              Cancel
            </button>
            <button type="submit" className="button button--primary" disabled={!password || reset.isPending}>
              Set password
            </button>
          </div>
        </form>
      )}
    </Dialog>
  );
}

/* ---------------------------------------------------------------- sessions */

interface SessionListProps {
  sessions: SessionInfo[];
  onEnd: (s: SessionInfo) => void;
  busy?: boolean;
}

export function SessionList({ sessions, onEnd, busy }: SessionListProps) {
  const now = useNow();
  if (sessions.length === 0) return <p className="meta">No active sessions.</p>;
  return (
    <ul className="session-list" aria-busy={busy || undefined}>
      {sessions.map((s) => (
        <li key={s.id} className="session-list__row">
          <span className="cell-stack">
            <span className="cell-strong">
              {describeUserAgent(s.user_agent)} {s.current && <Pill tone="ok">This session</Pill>}
            </span>
            <span className="meta">
              {s.ip ?? 'unknown address'} · signed in {formatDate(s.created_at)} · active {relativeTime(s.last_seen_at, now).toLowerCase()}
              {s.remember ? ' · remembered' : ''}
            </span>
          </span>
          <button type="button" className="button button--secondary button--sm" onClick={() => onEnd(s)} aria-label={s.current ? 'Sign out of this session' : `End session from ${s.ip ?? 'unknown address'}`}>
            {s.current ? 'Sign out' : 'End'}
          </button>
        </li>
      ))}
    </ul>
  );
}

/* ---------------------------------------------------------------- drawer */

type DrawerTab = 'overview' | 'tokens' | 'sessions' | 'permissions';

interface UserDrawerProps {
  id: string;
  canWrite: boolean;
  canDelete: boolean;
  canManageTokens: boolean;
  onClose: () => void;
}

/** Everything about one member or service account: roles, tokens, sessions, effective permissions. */
export function UserDrawer({ id, canWrite, canDelete, canManageTokens, onClose }: UserDrawerProps) {
  const now = useNow();
  const session = useSession();
  const member = useMember(id);
  const [tab, setTab] = useState<DrawerTab>('overview');
  const [dialog, setDialog] = useState<'edit' | 'reset' | 'delete' | 'disable' | 'endAll' | 'token' | null>(null);
  const [notice, setNotice] = useState('');
  const setDisabled = useSetDisabled();
  const unlock = useUnlock();
  const del = useDeleteMember();
  const endAll = useEndUserSessions();
  const m = member.data;
  const self = m?.id === session.data?.user.id;
  const sa = m?.kind === 'service_account';
  const status = m ? userStatus(m, now) : 'active';

  const tabs: { id: DrawerTab; label: string }[] = [
    { id: 'overview', label: 'Overview' },
    { id: 'tokens', label: 'Tokens' },
    ...(sa ? [] : [{ id: 'sessions' as const, label: 'Sessions' }]),
    { id: 'permissions', label: 'Permissions' },
  ];

  return (
    <Dialog title={m ? m.display_name || m.username : 'Loading…'} description={m ? (sa ? 'Service account' : m.username) : undefined} onClose={onClose} drawer>
      {member.isError && <FormError error={member.error} />}
      {m && (
        <div className="drawer">
          <div className="drawer__status">
            <Avatar name={m.display_name || m.username} size={44} />
            <StatusDot tone={USER_STATUS_META[status].tone} label={USER_STATUS_META[status].label} />
            {status === 'locked' && m.locked_until && <span className="meta">until {formatDate(m.locked_until)}</span>}
          </div>
          {notice && <Notice message={notice} />}
          {canWrite && (
            <div className="drawer__actions">
              <button type="button" className="button button--secondary button--sm" onClick={() => setDialog('edit')}>
                Edit
              </button>
              {status === 'locked' && (
                <button type="button" className="button button--secondary button--sm" onClick={() => unlock.mutate(m.id, { onSuccess: () => setNotice('Account unlocked.') })}>
                  Unlock
                </button>
              )}
              {m.source === 'local' && !sa && !self && (
                <button type="button" className="button button--secondary button--sm" onClick={() => setDialog('reset')}>
                  Reset password
                </button>
              )}
              {!sa && !self && (
                <button type="button" className="button button--secondary button--sm" onClick={() => setDialog('endAll')}>
                  Sign out everywhere
                </button>
              )}
              {!self &&
                (m.disabled ? (
                  <button type="button" className="button button--secondary button--sm" onClick={() => setDisabled.mutate({ id: m.id, disabled: false }, { onSuccess: () => setNotice('Account enabled.') })}>
                    Enable
                  </button>
                ) : (
                  <button type="button" className="button button--secondary button--sm" onClick={() => setDialog('disable')}>
                    Disable
                  </button>
                ))}
              {canDelete && !self && (
                <button type="button" className="button button--danger-ghost button--sm" onClick={() => setDialog('delete')}>
                  Delete
                </button>
              )}
            </div>
          )}
          <FormError error={unlock.error ?? setDisabled.error} />

          <div className="tabs" role="tablist" aria-label="Details">
            {tabs.map((t) => (
              <button
                key={t.id}
                type="button"
                role="tab"
                id={`drawer-tab-${t.id}`}
                aria-selected={tab === t.id}
                aria-controls={`drawer-panel-${t.id}`}
                className={tab === t.id ? 'tabs__item is-active' : 'tabs__item'}
                onClick={() => setTab(t.id)}
              >
                {t.label}
              </button>
            ))}
          </div>
          <div role="tabpanel" id={`drawer-panel-${tab}`} aria-labelledby={`drawer-tab-${tab}`} className="drawer__panel">
            {tab === 'overview' && <Overview member={m} />}
            {tab === 'tokens' && (
              <OwnerTokens owner={m.username} canCreate={sa && canManageTokens} onCreate={() => setDialog('token')} />
            )}
            {tab === 'sessions' && <UserSessions id={m.id} canWrite={canWrite} />}
            {tab === 'permissions' && <PermissionsReport id={m.id} />}
          </div>
        </div>
      )}
      {m && dialog === 'edit' && <MemberDialog kind={m.kind} member={m} onClose={() => setDialog(null)} />}
      {m && dialog === 'reset' && <ResetPasswordDialog member={m} onClose={() => setDialog(null)} />}
      {m && dialog === 'token' && <CreateTokenDialog owner={m.username} onClose={() => setDialog(null)} />}
      {m && dialog === 'disable' && (
        <ConfirmDialog
          title={`Disable ${m.display_name || m.username}?`}
          confirmLabel="Disable account"
          danger
          onConfirm={() => setDisabled.mutateAsync({ id: m.id, disabled: true }).then(() => setNotice('Account disabled.'))}
          onClose={() => setDialog(null)}
        >
          They are signed out at once and their tokens stop working until you enable the account again. Nothing is deleted.
        </ConfirmDialog>
      )}
      {m && dialog === 'endAll' && (
        <ConfirmDialog
          title={`Sign ${m.display_name || m.username} out everywhere?`}
          confirmLabel="End all sessions"
          onConfirm={() => endAll.mutateAsync(m.id).then((r) => setNotice(`${r.ended} session${r.ended === 1 ? '' : 's'} ended.`))}
          onClose={() => setDialog(null)}
        >
          Every browser session ends and short-lived registry logins are revoked. Personal access tokens keep working; revoke them under Tokens.
        </ConfirmDialog>
      )}
      {m && dialog === 'delete' && (
        <ConfirmDialog
          title={`Delete ${m.username}?`}
          confirmLabel="Delete permanently"
          danger
          typeToConfirm={m.username}
          onConfirm={async () => {
            await del.mutateAsync(m.id);
            onClose();
          }}
          onClose={() => setDialog(null)}
        >
          The account, its {m.token_count} token{m.token_count === 1 ? '' : 's'}, sessions and role bindings are deleted. The audit log keeps its history. To keep the
          account but block it, disable it instead.
        </ConfirmDialog>
      )}
    </Dialog>
  );
}

function Overview({ member: m }: { member: Member }) {
  const now = useNow();
  return (
    <dl className="summary__list">
      <dt>{m.kind === 'service_account' ? 'Name' : 'Username'}</dt>
      <dd className="mono">{m.username}</dd>
      {m.email && (
        <>
          <dt>Email</dt>
          <dd>{m.email}</dd>
        </>
      )}
      {m.description && (
        <>
          <dt>Used by</dt>
          <dd>{m.description}</dd>
        </>
      )}
      <dt>Signs in with</dt>
      <dd>{m.kind === 'service_account' ? 'Tokens only' : `${SOURCE_LABEL[m.source] ?? m.source}${m.source === 'local' && !m.password_set ? ' (no password set)' : ''}`}</dd>
      <dt>Roles</dt>
      <dd>
        {m.roles.length ? (
          <span className="pill-row">
            {m.roles.map((r) => (
              <Pill key={r} tone={roleTone(r)}>
                {r}
              </Pill>
            ))}
          </span>
        ) : (
          'None'
        )}
      </dd>
      <dt>Live tokens</dt>
      <dd>{m.token_count}</dd>
      <dt>Last active</dt>
      <dd>{m.last_active_at ? relativeTime(m.last_active_at, now) : 'Never'}</dd>
      {m.created_at && (
        <>
          <dt>Created</dt>
          <dd>{formatDate(m.created_at)}</dd>
        </>
      )}
    </dl>
  );
}

function OwnerTokens({ owner, canCreate, onCreate }: { owner: string; canCreate: boolean; onCreate: () => void }) {
  const tokens = useTokens({ owner, limit: 100 });
  return (
    <div className="drawer__section">
      {canCreate && (
        <button type="button" className="button button--secondary button--sm" onClick={onCreate}>
          New token
        </button>
      )}
      {tokens.isError ? (
        <FormError error={tokens.error} />
      ) : (
        <TokenTable caption={`Tokens of ${owner}`} tokens={tokens.data?.items ?? []} busy={tokens.isFetching} empty={tokens.isPending ? 'Loading…' : 'No tokens.'} />
      )}
    </div>
  );
}

function UserSessions({ id, canWrite }: { id: string; canWrite: boolean }) {
  const sessions = useUserSessions(id);
  const end = useEndUserSession();
  if (sessions.isError) return <FormError error={sessions.error} />;
  if (!sessions.data) return <p className="meta">Loading…</p>;
  return (
    <div className="drawer__section">
      <SessionList sessions={sessions.data.items} busy={end.isPending} onEnd={(s) => canWrite && end.mutate({ id, sid: s.id })} />
      <FormError error={end.error} />
    </div>
  );
}

const ACTION_SHORT: Record<string, string> = Object.fromEntries(ACTIONS.map((a) => [a.id, a.label]));

/** Effective permissions: per repository and area, the allowed actions and why. */
export function PermissionsReport({ id }: { id: string }) {
  const report = usePermissions(id);
  const [filter, setFilter] = useState('');
  const filterId = useId();
  if (report.isError) return <FormError error={report.error} />;
  const r = report.data;
  if (!r) return <p className="meta">Loading…</p>;
  const repos = r.repositories.filter((x) => !filter || x.name.includes(filter.trim().toLowerCase()));
  const why = (grants: (typeof r.repositories)[number]['grants'], action: string) =>
    grants
      .filter((g) => g.action === action)
      .map((g) => (g.source === 'anonymous' ? 'anonymous read' : `${g.role} (${g.privilege})`))
      .join(', ');
  return (
    <div className="drawer__section">
      {(r.disabled || r.locked) && (
        <p className="warn-note">
          {r.disabled ? 'This account is disabled' : 'This account is locked'}: none of this applies right now.
        </p>
      )}
      <h3 className="dialog-section__title">Administration</h3>
      {r.system.length === 0 ? (
        <p className="meta">No administrative rights.</p>
      ) : (
        <ul className="perm-list">
          {r.system.map((s) => (
            <li key={s.resource} className="perm-list__row">
              <span className="cell-strong">{SYSTEM_AREAS.find((a) => a.id === s.name)?.label ?? s.name}</span>
              <span className="pill-row">
                {s.actions.map((a) => (
                  <Pill key={a} tone="info">
                    <span title={why(s.grants, a)}>{ACTION_SHORT[a] ?? a}</span>
                  </Pill>
                ))}
              </span>
            </li>
          ))}
        </ul>
      )}
      <h3 className="dialog-section__title">Repositories ({r.repositories.length})</h3>
      {r.repositories.length > 8 && (
        <>
          <label className="visually-hidden" htmlFor={filterId}>
            Filter repositories
          </label>
          <input id={filterId} className="input input--sm" placeholder="Filter repositories" value={filter} onChange={(e) => setFilter(e.target.value)} />
        </>
      )}
      {r.repositories.length === 0 ? (
        <p className="meta">No access to any repository.</p>
      ) : (
        <ul className="perm-list">
          {repos.map((x) => (
            <li key={x.resource} className="perm-list__row">
              <span className="cell-stack">
                <span className="cell-strong mono">{x.name}</span>
                <span className="meta">
                  {x.format} · {x.type}
                  {x.anonymous_read ? ' · anonymous read' : ''}
                </span>
              </span>
              <span className="cell-stack perm-list__grants">
                <span className="pill-row">
                  {x.actions.map((a) => (
                    <Pill key={a} tone={a === 'read' ? 'neutral' : a === 'admin' ? 'violet' : 'info'}>
                      {ACTION_SHORT[a] ?? a}
                    </Pill>
                  ))}
                </span>
                <span className="meta">{x.actions.map((a) => `${ACTION_SHORT[a]}: ${why(x.grants, a)}`).join(' · ')}</span>
              </span>
            </li>
          ))}
        </ul>
      )}
      {r.notes.map((n) => (
        <p key={n} className="meta">
          {n}
        </p>
      ))}
    </div>
  );
}
