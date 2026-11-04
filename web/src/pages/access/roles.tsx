import { useId, useState, type FormEvent } from 'react';
import { DataTable, type Column } from '../../components/DataTable';
import { ErrorNote } from '../../components/ErrorNote';
import { IconClose, IconPlus } from '../../components/icons';
import { Panel } from '../../components/Panel';
import { Pill } from '../../components/Pill';
import { Select } from '../../components/Select';
import {
  useAddRoleMember,
  useCreateRole,
  useDeleteRole,
  useMembers,
  useRemoveRoleMember,
  useRoles,
  useUpdateRole,
  type Role,
  type RoleInput,
} from './api';
import { ACTIONS, describePrivilege, formatPrivilege, parsePrivilege, repoPatternError, ROLE_NAME_RE, roleTone, SYSTEM_AREAS, type Privilege } from './model';
import { ScopeList } from './tokens';
import { ConfirmDialog, Dialog, Field, FormError } from './ui';

const DOMAIN_OPTIONS = [
  { value: 'repo', label: 'Repositories' },
  { value: 'system', label: 'Administration' },
] as const;
const AREA_OPTIONS = [{ value: '*', label: 'Every area' }, ...SYSTEM_AREAS.map((a) => ({ value: a.id as string, label: a.label }))];

/* ---------------------------------------------------------------- privilege editor */

interface PrivilegeEditorProps {
  value: string[];
  onChange: (v: string[]) => void;
  readOnly?: boolean;
  showErrors?: boolean;
}

/** Edits a list of privileges as rows: repositories (pattern × action) or a system area × action. */
export function PrivilegeEditor({ value, onChange, readOnly, showErrors }: PrivilegeEditorProps) {
  const rows = value.map((s) => parsePrivilege(s) ?? ({ domain: 'repo', pattern: '', action: 'read' } as Privilege));
  const set = (i: number, p: Privilege) => onChange(value.map((v, j) => (j === i ? formatPrivilege(p) : v)));
  const remove = (i: number) => onChange(value.filter((_, j) => j !== i));
  const baseId = useId();

  if (readOnly) {
    return (
      <ul className="privilege-list">
        {value.map((s) => (
          <li key={s}>
            <code className="scope">{s}</code> <span className="meta">{describePrivilege(s)}</span>
          </li>
        ))}
      </ul>
    );
  }
  return (
    <div className="privilege-editor">
      {rows.length === 0 && <p className="meta">No privileges yet: members of this role can do nothing.</p>}
      {rows.map((p, i) => {
        const err = p.domain === 'repo' ? repoPatternError(p.pattern) : null;
        const id = `${baseId}-${i}`;
        return (
          <div key={i} className="privilege-row" role="group" aria-label={`Privilege ${i + 1}`}>
            <Select
              size="sm"
              aria-label="Applies to"
              value={p.domain}
              options={DOMAIN_OPTIONS}
              onChange={(v) => set(i, v === 'repo' ? { domain: 'repo', pattern: '*', action: 'read' } : { domain: 'system', pattern: 'audit', action: 'read' })}
            />
            {p.domain === 'repo' ? (
              <input
                id={id}
                className="input input--sm input--mono"
                aria-label="Repository name or pattern"
                value={p.pattern}
                onChange={(e) => set(i, { ...p, pattern: e.target.value })}
                aria-invalid={showErrors && !!err}
                placeholder="npm-*"
                spellCheck={false}
              />
            ) : (
              <Select size="sm" aria-label="Area" value={p.pattern} options={AREA_OPTIONS} onChange={(v) => set(i, { ...p, pattern: v })} />
            )}
            <Select<Privilege['action']>
              size="sm"
              aria-label="Action"
              value={p.action === '*' ? 'admin' : p.action}
              options={ACTIONS.map((a) => ({ value: a.id, label: a.label }))}
              onChange={(v) => set(i, { ...p, action: v })}
            />
            <button type="button" className="icon-button icon-button--sm" aria-label={`Remove privilege ${i + 1}`} onClick={() => remove(i)}>
              <IconClose size={16} />
            </button>
            <span className={showErrors && err ? 'field__error privilege-row__note' : 'meta privilege-row__note'}>
              {showErrors && err ? err : describePrivilege(value[i] ?? '')}
            </span>
          </div>
        );
      })}
      <div className="chip-row">
        <button type="button" className="button button--secondary button--sm" onClick={() => onChange([...value, 'repo:*:read'])}>
          <IconPlus size={14} /> Repository privilege
        </button>
        <button type="button" className="button button--secondary button--sm" onClick={() => onChange([...value, 'system:audit:read'])}>
          <IconPlus size={14} /> Administration privilege
        </button>
      </div>
    </div>
  );
}

/* ---------------------------------------------------------------- roles tab */

export function RolesTab({ canWrite }: { canWrite: boolean }) {
  const roles = useRoles();
  const [open, setOpen] = useState<Role | 'new' | null>(null);

  const columns: Column<Role>[] = [
    {
      key: 'name',
      header: 'Role',
      className: 'col-first',
      cell: (r) => (
        <button type="button" className="link-button" onClick={() => setOpen(r)}>
          <Pill tone={roleTone(r.name)}>{r.name}</Pill>
        </button>
      ),
    },
    { key: 'desc', header: 'Description', className: 'muted', cell: (r) => r.description || <span className="meta">—</span> },
    { key: 'privs', header: 'Privileges', cell: (r) => (r.privileges.length === 0 ? <span className="meta">None</span> : <ScopeList scopes={r.privileges} max={2} />) },
    { key: 'members', header: 'Members', align: 'right', className: 'nowrap', cell: (r) => r.member_count },
    {
      key: 'groups',
      header: 'Groups',
      align: 'right',
      className: 'nowrap',
      cell: (r) => (r.group_mappings?.length ? r.group_mappings.length : <span className="meta">—</span>),
    },
    { key: 'kind', header: 'Type', className: 'col-last', cell: (r) => (r.builtin ? <span className="meta">Built-in</span> : 'Custom') },
  ];

  return (
    <Panel flush aria-label="Roles" title="Roles" titleId="roles-title" action={
      canWrite ? (
        <button type="button" className="button button--secondary button--sm" onClick={() => setOpen('new')}>
          <IconPlus size={14} /> New role
        </button>
      ) : undefined
    }>
      {roles.isError ? (
        <ErrorNote what="roles" onRetry={() => void roles.refetch()} />
      ) : (
        <DataTable caption="Roles" columns={columns} rows={roles.data?.items ?? []} rowKey={(r) => r.id} busy={roles.isFetching} minWidth={760} empty={roles.isPending ? 'Loading…' : 'No roles.'} />
      )}
      {open && <RoleDialog role={open === 'new' ? null : open} canWrite={canWrite} onClose={() => setOpen(null)} />}
    </Panel>
  );
}

function RoleDialog({ role, canWrite, onClose }: { role: Role | null; canWrite: boolean; onClose: () => void }) {
  const editable = canWrite && !role?.builtin;
  const [name, setName] = useState(role?.name ?? '');
  const [description, setDescription] = useState(role?.description ?? '');
  const [privileges, setPrivileges] = useState<string[]>(role?.privileges ?? ['repo:*:read']);
  const [groups, setGroups] = useState(role?.group_mappings ?? []);
  const [submitted, setSubmitted] = useState(false);
  const [confirmDelete, setConfirmDelete] = useState(false);
  const create = useCreateRole();
  const update = useUpdateRole();
  const del = useDeleteRole();
  const ids = { name: useId(), desc: useId() };
  const save = role ? update : create;

  const nameError = !ROLE_NAME_RE.test(name) ? 'Letters, digits, spaces, dots, dashes and underscores; up to 64 characters.' : null;
  const privError = privileges.some((s) => {
    const p = parsePrivilege(s);
    return !p || (p.domain === 'repo' && repoPatternError(p.pattern));
  });
  const groupError = groups.some((g) => !g.provider.trim() || !g.group.trim());

  const submit = (e: FormEvent) => {
    e.preventDefault();
    setSubmitted(true);
    if (nameError || privError || groupError) return;
    const body: RoleInput = { name, description, privileges, group_mappings: groups.map((g) => ({ provider: g.provider.trim(), group: g.group.trim() })) };
    if (role) update.mutate({ name: role.name, body }, { onSuccess: onClose });
    else create.mutate(body, { onSuccess: onClose });
  };

  return (
    <Dialog
      title={role ? (editable ? `Edit role ${role.name}` : `Role ${role.name}`) : 'New role'}
      description={role?.builtin ? 'Built-in roles cannot be changed, but you can grant them to people.' : 'A role is a set of privileges. Grant it to people, service accounts or directory groups.'}
      onClose={onClose}
      drawer
    >
      <form className="dialog-form" onSubmit={submit} noValidate>
        {editable && (
          <>
            <Field id={ids.name} label="Name" error={submitted && nameError}>
              <input className="input" value={name} onChange={(e) => setName(e.target.value)} data-autofocus />
            </Field>
            <Field id={ids.desc} label={<>Description <span className="field__optional">(optional)</span></>}>
              <input className="input" value={description} onChange={(e) => setDescription(e.target.value)} maxLength={1000} />
            </Field>
          </>
        )}
        <section className="dialog-section" aria-label="Privileges">
          <h3 className="dialog-section__title">Privileges</h3>
          <PrivilegeEditor value={privileges} onChange={setPrivileges} readOnly={!editable} showErrors={submitted} />
        </section>
        <section className="dialog-section" aria-label="Directory groups">
          <h3 className="dialog-section__title">Directory groups</h3>
          <p className="meta">Members of these OIDC, LDAP or SAML groups get the role when they sign in.</p>
          {groups.length === 0 && <p className="meta">No groups mapped.</p>}
          {groups.map((g, i) => (
            <div key={i} className="privilege-row privilege-row--groups">
              <input
                className="input input--sm"
                aria-label={`Provider ${i + 1}`}
                placeholder="Provider name"
                value={g.provider}
                readOnly={!editable}
                onChange={(e) => setGroups(groups.map((x, j) => (j === i ? { ...x, provider: e.target.value } : x)))}
              />
              <input
                className="input input--sm"
                aria-label={`Group ${i + 1}`}
                placeholder="Group, e.g. platform-team"
                value={g.group}
                readOnly={!editable}
                onChange={(e) => setGroups(groups.map((x, j) => (j === i ? { ...x, group: e.target.value } : x)))}
              />
              {editable && (
                <button type="button" className="icon-button icon-button--sm" aria-label={`Remove group ${i + 1}`} onClick={() => setGroups(groups.filter((_, j) => j !== i))}>
                  <IconClose size={16} />
                </button>
              )}
            </div>
          ))}
          {submitted && groupError && <p className="field__error">Every mapping needs a provider and a group.</p>}
          {editable && (
            <button type="button" className="button button--secondary button--sm" onClick={() => setGroups([...groups, { provider: '', group: '' }])}>
              <IconPlus size={14} /> Map a group
            </button>
          )}
        </section>
        {role && <RoleMembers role={role.name} canWrite={canWrite} />}
        <FormError error={save.error} />
        {editable && (
          <div className="dialog-actions">
            {role && (
              <button type="button" className="button button--danger-ghost" onClick={() => setConfirmDelete(true)}>
                Delete role
              </button>
            )}
            <button type="button" className="button button--secondary" onClick={onClose}>
              Cancel
            </button>
            <button type="submit" className="button button--primary" disabled={save.isPending}>
              {save.isPending ? 'Saving…' : role ? 'Save role' : 'Create role'}
            </button>
          </div>
        )}
      </form>
      {confirmDelete && role && (
        <ConfirmDialog
          title={`Delete role ${role.name}?`}
          confirmLabel="Delete role"
          danger
          typeToConfirm={role.name}
          onConfirm={async () => {
            await del.mutateAsync(role.name);
            onClose();
          }}
          onClose={() => setConfirmDelete(false)}
        >
          {role.member_count} member{role.member_count === 1 ? '' : 's'} and every group mapping lose the role and its privileges at once.
        </ConfirmDialog>
      )}
    </Dialog>
  );
}

function RoleMembers({ role, canWrite }: { role: string; canWrite: boolean }) {
  const members = useMembers({ role, limit: 200 });
  const everyone = useMembers({ limit: 1000 }, canWrite);
  const add = useAddRoleMember();
  const remove = useRemoveRoleMember();
  const [pick, setPick] = useState('');
  const pickId = useId();
  const current = new Set((members.data?.items ?? []).map((m) => m.id));
  const candidates = (everyone.data?.items ?? []).filter((m) => !current.has(m.id));

  return (
    <section className="dialog-section" aria-label="Members">
      <h3 className="dialog-section__title">Members</h3>
      {members.isPending && <p className="meta">Loading…</p>}
      {members.data && members.data.items.length === 0 && <p className="meta">Nobody has this role directly.</p>}
      <ul className="members">
        {(members.data?.items ?? []).map((m) => (
          <li key={m.id} className="members__row">
            <span className="members__name">{m.display_name || m.username}</span>
            <span className="meta">{m.kind === 'service_account' ? 'service account' : m.username}</span>
            {canWrite && (
              <button
                type="button"
                className="icon-button icon-button--sm"
                aria-label={`Remove ${m.username} from ${role}`}
                onClick={() => remove.mutate({ role, userId: m.id })}
              >
                <IconClose size={16} />
              </button>
            )}
          </li>
        ))}
      </ul>
      {canWrite && (
        <div className="members__add">
          <label className="visually-hidden" htmlFor={pickId}>
            Add a member
          </label>
          <Select
            id={pickId}
            value={pick}
            placeholder="Add a member…"
            options={candidates.map((m) => ({ value: m.id, label: m.display_name || m.username, hint: m.display_name ? m.username : undefined }))}
            onChange={setPick}
          />
          <button
            type="button"
            className="button button--secondary"
            disabled={!pick || add.isPending}
            onClick={() => add.mutate({ role, userId: pick }, { onSuccess: () => setPick('') })}
          >
            Add
          </button>
        </div>
      )}
      <FormError error={add.error ?? remove.error} />
    </section>
  );
}
