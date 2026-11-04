import { useId, useMemo, useState, type FormEvent } from 'react';
import { useSession } from '../../api/hooks';
import { DataTable, type Column } from '../../components/DataTable';
import { Pill } from '../../components/Pill';
import { Select } from '../../components/Select';
import { relativeTime } from '../../lib/format';
import { useNow } from '../../lib/useNow';
import { useCreateToken, usePermissions, useRepositoryNames, useRevokeToken, useRotateToken, type Token, type TokenCreated } from './api';
import {
  ACTIONS,
  allows,
  customExpiry,
  describePrivilege,
  expiresInDays,
  expiryPresets,
  expiryText,
  formatDate,
  parseGoDuration,
  repoPatternError,
  SYSTEM_AREAS,
  TOKEN_STATE_META,
  tokenState,
  uncoveredScopes,
  type Action,
} from './model';
import {
  DEFAULT_DRAFT,
  draftScopes,
  levelAllowed,
  matchingRepos,
  networkError,
  ownerCanRepo,
  ownerHasRepoAction,
  ownerLevel,
  parseNetworks,
  presetAvailability,
  rotatedLifetimeDays,
  shortSummary,
  summarizeScopes,
  SYSTEM_LEVELS,
  TOKEN_PRESETS,
  type PermissionDraft,
  type SystemLevel,
} from './tokenScopes';
import { SecretPanel } from './TokenReveal';
import { ConfirmDialog, Dialog, Field, FormError } from './ui';

/* ---------------------------------------------------------------- list */

export function ScopeList({ scopes, max = 3 }: { scopes: string[]; max?: number }) {
  const shown = scopes.slice(0, max);
  const more = scopes.length - shown.length;
  return (
    <span className="scope-list">
      {shown.map((s) => (
        <code key={s} className="scope" title={describePrivilege(s)}>
          {s}
        </code>
      ))}
      {more > 0 && (
        <span className="meta" title={scopes.slice(max).join(', ')}>
          +{more} more
        </span>
      )}
    </span>
  );
}

interface TokenTableProps {
  caption: string;
  tokens: Token[];
  showOwner?: boolean;
  busy?: boolean;
  empty?: string;
}

/** Tokens with what they can do, restrictions, last use (time and address), expiry, Rotate and Revoke. */
export function TokenTable({ caption, tokens, showOwner, busy, empty }: TokenTableProps) {
  const now = useNow();
  const session = useSession();
  const warnMs = parseGoDuration(session.data?.token_policy?.expiry_warning) ?? undefined;
  const me = session.data?.user.username;
  const manageServiceTokens = allows(session.data?.privileges, 'system', 'tokens', 'write');
  const viaReadOnlyToken = session.data?.token?.read_only === true;
  const [revoking, setRevoking] = useState<Token | null>(null);
  const [rotating, setRotating] = useState<Token | null>(null);
  const revoke = useRevokeToken();
  const canRotate = (t: Token) => !viaReadOnlyToken && (t.owner === me || (t.owner_kind === 'service_account' && manageServiceTokens));

  const columns: Column<Token>[] = [
    {
      key: 'name',
      header: 'Token',
      className: 'col-first',
      cell: (t) => (
        <div className="cell-stack">
          <span className="cell-strong">{t.name}</span>
          <code className="meta mono">{t.prefix}…</code>
        </div>
      ),
    },
    ...(showOwner
      ? [
          {
            key: 'owner',
            header: 'Owner',
            cell: (t: Token) => (
              <div className="cell-stack">
                <span>{t.owner}</span>
                <span className="meta">{t.owner_kind === 'service_account' ? 'Service account' : 'Person'}</span>
              </div>
            ),
          },
        ]
      : []),
    {
      key: 'scopes',
      header: 'Can',
      cell: (t) => (
        <div className="cell-stack">
          <span title={t.scopes.map(describePrivilege).join('\n')}>{shortSummary(t.scopes, t.read_only)}</span>
          {(t.read_only || t.allowed_cidrs.length > 0) && (
            <span className="pill-row">
              {t.read_only && <Pill tone="info">Read-only</Pill>}
              {t.allowed_cidrs.length > 0 && (
                <span title={`Works only from ${t.allowed_cidrs.join(', ')}`}>
                  <Pill tone="sage">{t.allowed_cidrs.length === 1 ? t.allowed_cidrs[0] : `${t.allowed_cidrs.length} networks`}</Pill>
                </span>
              )}
            </span>
          )}
        </div>
      ),
    },
    { key: 'created', header: 'Created', className: 'meta', cell: (t) => formatDate(t.created_at) },
    {
      key: 'used',
      header: 'Last used',
      cell: (t) =>
        t.last_used_at ? (
          <div className="cell-stack">
            <span>{relativeTime(t.last_used_at, now)}</span>
            {t.last_used_ip && <span className="meta mono">{t.last_used_ip}</span>}
          </div>
        ) : (
          <span className="meta">Never</span>
        ),
    },
    {
      key: 'expires',
      header: 'Expires',
      cell: (t) => {
        const st = tokenState(t, now, warnMs);
        return (
          <div className="cell-stack">
            <Pill tone={TOKEN_STATE_META[st].tone}>{TOKEN_STATE_META[st].label}</Pill>
            <span className="meta" title={t.expires_at}>
              {formatDate(t.expires_at)} · {expiryText(t.expires_at, now)}
            </span>
          </div>
        );
      },
    },
    {
      key: 'actions',
      header: <span className="visually-hidden">Actions</span>,
      align: 'right',
      className: 'col-last',
      cell: (t) => (
        <span className="row-actions">
          {canRotate(t) && (
            <button type="button" className="button button--secondary button--sm" onClick={() => setRotating(t)} aria-label={`Rotate token ${t.name}`}>
              Rotate
            </button>
          )}
          <button type="button" className="button button--secondary button--sm" onClick={() => setRevoking(t)} aria-label={`Revoke token ${t.name}`}>
            Revoke
          </button>
        </span>
      ),
    },
  ];

  return (
    <>
      <DataTable caption={caption} columns={columns} rows={tokens} rowKey={(t) => t.id} busy={busy} minWidth={showOwner ? 1020 : 880} empty={empty ?? 'No tokens.'} />
      {revoking && (
        <ConfirmDialog
          title={`Revoke “${revoking.name}”?`}
          confirmLabel="Revoke token"
          danger
          onConfirm={() => revoke.mutateAsync(revoking.id)}
          onClose={() => setRevoking(null)}
        >
          Anything using <code className="inline-code">{revoking.prefix}…</code>
          {revoking.owner_kind === 'service_account' ? ` (service account ${revoking.owner})` : ''} stops working at once,
          including registry logins made with it. This cannot be undone.
        </ConfirmDialog>
      )}
      {rotating && <RotateDialog token={rotating} onClose={() => setRotating(null)} />}
    </>
  );
}

/* ---------------------------------------------------------------- rotate */

type OldTokenFate = 'keep' | 'later' | 'now';

const GRACE_OPTIONS = [
  { value: '1h', label: 'In 1 hour' },
  { value: '24h', label: 'In 24 hours' },
  { value: '72h', label: 'In 3 days' },
  { value: '168h', label: 'In 7 days' },
];

export function RotateDialog({ token, onClose }: { token: Token; onClose: () => void }) {
  const session = useSession();
  const maxDays = session.data?.token_policy?.max_lifetime_days ?? 365;
  const [fate, setFate] = useState<OldTokenFate>('later');
  const [grace, setGrace] = useState('24h');
  const [created, setCreated] = useState<{ token: TokenCreated; note: string } | null>(null);
  const rotate = useRotateToken();
  const graceId = useId();
  const days = rotatedLifetimeDays(token, maxDays);

  const submit = (e: FormEvent) => {
    e.preventDefault();
    const revoke_old_after = fate === 'keep' ? null : fate === 'now' ? '0s' : grace;
    rotate.mutate(
      { id: token.id, body: { revoke_old_after } },
      {
        onSuccess: (r) => {
          const old = r.replaced;
          const note = old.revoked
            ? `The old token ${token.prefix}… is revoked.`
            : `The old token ${token.prefix}… keeps working until ${formatDate(old.expires_at)} (${expiryText(old.expires_at)}).`;
          setCreated({ token: r, note });
        },
      },
    );
  };

  return (
    <Dialog
      title={created ? 'Copy the new token now' : `Rotate “${token.name}”?`}
      description={
        created
          ? 'This is the only time the secret is shown. Replace the old token wherever it is used.'
          : `A new secret with the same name, permissions and restrictions, valid for ${days} day${days === 1 ? '' : 's'}.`
      }
      onClose={onClose}
      wide={!!created}
    >
      {created ? (
        <SecretPanel token={created.token} note={created.note} onDone={onClose} />
      ) : (
        <form className="dialog-form" onSubmit={submit}>
          <fieldset className="fieldset">
            <legend className="field__label">The old token {token.prefix}…</legend>
            <div className="option-grid option-grid--stack">
              <label className={fate === 'later' ? 'option is-on' : 'option'}>
                <input type="radio" name="fate" checked={fate === 'later'} onChange={() => setFate('later')} />
                <span>
                  <span className="option__title">Stops working after a grace period</span>
                  <span className="option__hint">Time to roll the new secret out everywhere.</span>
                </span>
              </label>
              {fate === 'later' && (
                <Field id={graceId} label="Old token stops working">
                  <Select id={graceId} value={grace} options={GRACE_OPTIONS} onChange={setGrace} />
                </Field>
              )}
              <label className={fate === 'now' ? 'option is-on' : 'option'}>
                <input type="radio" name="fate" checked={fate === 'now'} onChange={() => setFate('now')} />
                <span>
                  <span className="option__title">Revoke it now</span>
                  <span className="option__hint">Use when the old secret may have leaked.</span>
                </span>
              </label>
              <label className={fate === 'keep' ? 'option is-on' : 'option'}>
                <input type="radio" name="fate" checked={fate === 'keep'} onChange={() => setFate('keep')} />
                <span>
                  <span className="option__title">Keep it until it expires</span>
                  <span className="option__hint">On {formatDate(token.expires_at)}; revoke it yourself when you are done.</span>
                </span>
              </label>
            </div>
          </fieldset>
          <FormError error={rotate.error} />
          <div className="dialog-actions">
            <button type="button" className="button button--secondary" onClick={onClose}>
              Cancel
            </button>
            <button type="submit" className="button button--primary" disabled={rotate.isPending}>
              {rotate.isPending ? 'Rotating…' : 'Create replacement'}
            </button>
          </div>
        </form>
      )}
    </Dialog>
  );
}

/* ---------------------------------------------------------------- create */

interface CreateTokenProps {
  /** A service account to create the token for; the caller when absent. */
  owner?: string;
  /** The service account's id, to read its privileges. */
  ownerId?: string;
  onClose: () => void;
}

export function CreateTokenDialog({ owner, ownerId, onClose }: CreateTokenProps) {
  const [created, setCreated] = useState<TokenCreated | null>(null);
  return (
    <Dialog
      title={created ? 'Copy your token now' : owner ? `New token for ${owner}` : 'New personal access token'}
      description={
        created
          ? 'This is the only time the secret is shown. Hangar stores only a hash of it.'
          : 'For scripts, CI jobs, package clients and other automation. A token never grants more than its owner’s roles.'
      }
      onClose={onClose}
      wide
    >
      {created ? <SecretPanel token={created} onDone={onClose} /> : <TokenForm owner={owner} ownerId={ownerId} onCreated={setCreated} onCancel={onClose} />}
    </Dialog>
  );
}

/** The privileges a new token is limited by: the caller's own, or the service account's roles. */
function useOwnerPrivileges(ownerId?: string): { privileges: string[] | undefined; whose: string } {
  const session = useSession();
  const perms = usePermissions(ownerId);
  if (ownerId) return { privileges: perms.data?.roles.flatMap((r) => r.privileges), whose: 'The service account’s roles' };
  return { privileges: session.data?.privileges, whose: session.data?.auth_method === 'token' ? 'The token you signed in with' : 'Your roles' };
}

function TokenForm({ owner, ownerId, onCreated, onCancel }: { owner?: string; ownerId?: string; onCreated: (t: TokenCreated) => void; onCancel: () => void }) {
  const session = useSession();
  const maxDays = session.data?.token_policy?.max_lifetime_days ?? 365;
  const mustReadOnly = session.data?.token?.read_only === true;
  const presets = expiryPresets(maxDays);
  const { privileges, whose } = useOwnerPrivileges(ownerId);
  const [name, setName] = useState('');
  const [expiry, setExpiry] = useState<number | 'custom'>(presets.includes(30) ? 30 : (presets[0] ?? 30));
  const [customDate, setCustomDate] = useState('');
  const [draft, setDraft] = useState<PermissionDraft>(DEFAULT_DRAFT);
  const [networks, setNetworks] = useState('');
  const [readOnly, setReadOnly] = useState(mustReadOnly);
  const [submitted, setSubmitted] = useState(false);
  const create = useCreateToken();
  const ids = { name: useId(), date: useId(), networks: useId() };

  const { scopes, error: scopeError } = draftScopes(draft);
  const custom = expiry === 'custom' ? customExpiry(customDate, maxDays) : null;
  const nameError = !name.trim() ? 'Give the token a name you will recognise.' : name.trim().length > 100 ? 'At most 100 characters.' : null;
  const cidrs = parseNetworks(networks);
  const networksError = cidrs.length > 32 ? 'At most 32 networks.' : (cidrs.map(networkError).find(Boolean) ?? null);
  const uncovered = privileges ? uncoveredScopes(scopes, privileges) : [];
  const summary = summarizeScopes(scopes, readOnly);
  const preset = TOKEN_PRESETS.find((p) => p.id === draft.preset)!;

  const submit = (e: FormEvent) => {
    e.preventDefault();
    setSubmitted(true);
    if (nameError || scopeError || networksError || (custom && custom.error)) return;
    const expiresAt = expiry === 'custom' ? custom!.value! : expiresInDays(expiry);
    create.mutate(
      { name: name.trim(), owner, scopes, expires_at: expiresAt, allowed_cidrs: cidrs, read_only: readOnly },
      { onSuccess: onCreated },
    );
  };

  return (
    <form className="dialog-form" onSubmit={submit} noValidate>
      <Field id={ids.name} label="Name" error={submitted && nameError}>
        <input
          className="input"
          value={name}
          onChange={(e) => setName(e.target.value)}
          placeholder={owner ? 'e.g. release pipeline' : 'e.g. laptop CLI'}
          maxLength={100}
          data-autofocus
        />
      </Field>

      <fieldset className="fieldset">
        <legend className="field__label">Expires</legend>
        <div className="chip-row">
          {presets.map((d) => (
            <label key={d} className={expiry === d ? 'chip is-on' : 'chip'}>
              <input type="radio" name="expiry" className="visually-hidden" checked={expiry === d} onChange={() => setExpiry(d)} />
              {d === 365 ? '1 year' : `${d} days`}
            </label>
          ))}
          <label className={expiry === 'custom' ? 'chip is-on' : 'chip'}>
            <input type="radio" name="expiry" className="visually-hidden" checked={expiry === 'custom'} onChange={() => setExpiry('custom')} />
            Custom date
          </label>
        </div>
        {expiry === 'custom' ? (
          <Field id={ids.date} label="Expiry date" error={submitted && custom?.error}>
            <input type="date" className="input" value={customDate} onChange={(e) => setCustomDate(e.target.value)} />
          </Field>
        ) : (
          <span className="field__hint">
            On {formatDate(expiresInDays(expiry))}. Tokens always expire; this server allows at most {maxDays} days.
          </span>
        )}
      </fieldset>

      <fieldset className="fieldset">
        <legend className="field__label">Permissions</legend>
        <div className="option-grid" role="radiogroup" aria-label="Permission preset">
          {TOKEN_PRESETS.map((p) => {
            const avail = privileges ? presetAvailability(p.id, privileges) : { ok: true };
            const on = draft.preset === p.id;
            return (
              <label
                key={p.id}
                className={['option', on ? 'is-on' : '', avail.ok ? '' : 'is-disabled', p.danger ? 'option--danger' : ''].filter(Boolean).join(' ')}
                title={avail.reason}
              >
                <input
                  type="radio"
                  name="preset"
                  checked={on}
                  disabled={!avail.ok}
                  onChange={() => setDraft((d) => ({ ...d, preset: p.id }))}
                />
                <span>
                  <span className="option__title">{p.label}</span>
                  <span className="option__hint">{avail.ok ? p.hint : avail.reason}</span>
                </span>
              </label>
            );
          })}
        </div>
        {draft.preset === 'admin' && (
          <p className="warn-note" role="note">
            A full-admin token can do everything you can: manage users, roles, tokens, repositories and settings. Give it a short expiry,
            restrict where it works from, and never put it in a repository or a shared CI variable.
          </p>
        )}
        {preset.picksRepos && (
          <RepoTargets
            value={draft.repos}
            onChange={(repos) => setDraft((d) => ({ ...d, repos }))}
            privileges={privileges}
            need={draft.preset === 'repo-admin' ? 'admin' : draft.preset === 'publish' ? 'write' : 'read'}
            whose={whose}
          />
        )}
        {draft.preset === 'custom' && <CustomEditor draft={draft} setDraft={setDraft} privileges={privileges} whose={whose} />}
      </fieldset>

      <details className="advanced" open={readOnly || networks !== ''}>
        <summary className="advanced__summary">Restrictions (optional)</summary>
        <div className="dialog-form">
          <label className="checkbox">
            <input type="checkbox" checked={readOnly} disabled={mustReadOnly} onChange={(e) => setReadOnly(e.target.checked)} />
            <span>
              Read-only: the token can only download and view, whatever its permissions say
              {mustReadOnly && <span className="meta"> (required: you signed in with a read-only token)</span>}
            </span>
          </label>
          <Field
            id={ids.networks}
            label="Works only from these addresses"
            error={submitted && networksError}
            hint="IP addresses or CIDR networks, one per line, e.g. 203.0.113.7 or 10.20.0.0/16. Empty: any address."
          >
            <textarea
              className="textarea input--mono"
              rows={3}
              value={networks}
              onChange={(e) => setNetworks(e.target.value)}
              spellCheck={false}
              placeholder={'10.20.0.0/16\n203.0.113.7'}
            />
          </Field>
        </div>
      </details>

      <div className="scope-preview" aria-live="polite" data-testid="token-summary">
        <p className="field__label">This token</p>
        {scopeError ? (
          <p className={submitted ? 'field__error' : 'meta'}>{scopeError}</p>
        ) : (
          <ul className="scope-preview__list">
            {summary.can.map((s) => (
              <li key={s}>{s}</li>
            ))}
            {summary.cannot.map((s) => (
              <li key={s} className="meta">
                {s}
              </li>
            ))}
          </ul>
        )}
        {!scopeError && (
          <p className="meta">
            Scopes: <ScopeList scopes={scopes} max={6} />
          </p>
        )}
        {uncovered.length > 0 && (
          <p className="warn-note">
            {whose} do not grant {uncovered.join(', ')}. The server only lets a token do what its owner may, so those permissions will have no
            effect unless the roles change.
          </p>
        )}
      </div>

      <FormError error={create.error} />
      <div className="dialog-actions">
        <button type="button" className="button button--secondary" onClick={onCancel}>
          Cancel
        </button>
        <button type="submit" className="button button--primary" disabled={create.isPending}>
          {create.isPending ? 'Creating…' : 'Create token'}
        </button>
      </div>
    </form>
  );
}

/* ---------------------------------------------------------------- repositories */

interface RepoTargetsProps {
  value: string[];
  onChange: (v: string[]) => void;
  privileges: string[] | undefined;
  /** The action the preset needs on each pick, to grey out what the owner lacks. */
  need: Action;
  whose: string;
}

/** Multi-select of repositories plus glob patterns (npm-*), with what each pattern matches. */
function RepoTargets({ value, onChange, privileges, need, whose }: RepoTargetsProps) {
  const names = useRepositoryNames(true);
  const [filter, setFilter] = useState('');
  const [pattern, setPattern] = useState('');
  const [patternError, setPatternError] = useState<string | null>(null);
  const ids = { filter: useId(), pattern: useId() };
  const visible = useMemo(() => (names.data ?? []).filter((r) => !filter || r.includes(filter.trim().toLowerCase())), [names.data, filter]);
  const toggle = (r: string) => onChange(value.includes(r) ? value.filter((x) => x !== r) : [...value, r]);
  const patterns = value.filter((v) => /[*?[]/.test(v));
  const verb = need === 'admin' ? 'manage' : need === 'write' ? 'publish to' : need === 'delete' ? 'delete from' : 'read';

  const addPattern = () => {
    const p = pattern.trim();
    const err = repoPatternError(p);
    if (err) {
      setPatternError(err);
      return;
    }
    setPatternError(null);
    if (!value.includes(p)) onChange([...value, p]);
    setPattern('');
  };

  return (
    <div className="repo-picker">
      {value.length > 0 && (
        <div className="chip-row" aria-label="Selected repositories">
          {value.map((v) => {
            const isGlob = patterns.includes(v);
            const matches = isGlob && names.data ? matchingRepos([v], names.data).length : null;
            return (
              <span key={v} className="target-chip">
                <span className="mono">{v}</span>
                {matches !== null && <span className="meta">{matches === 1 ? '1 match' : `${matches} matches`}</span>}
                <button type="button" className="target-chip__remove" aria-label={`Remove ${v}`} onClick={() => onChange(value.filter((x) => x !== v))}>
                  ×
                </button>
              </span>
            );
          })}
        </div>
      )}
      <label className="visually-hidden" htmlFor={ids.filter}>
        Filter repositories
      </label>
      <input id={ids.filter} className="input" placeholder="Filter repositories" value={filter} onChange={(e) => setFilter(e.target.value)} />
      <div className="repo-picker__list" role="group" aria-label="Repositories">
        {names.isPending && <p className="meta">Loading repositories…</p>}
        {names.isError && <p className="meta">Could not load repositories. Add them as patterns below.</p>}
        {visible.map((r) => {
          const ok = !privileges || ownerCanRepo(privileges, r, need);
          return (
            <label key={r} className={ok ? 'checkbox' : 'checkbox is-disabled'} title={ok ? undefined : `${whose} do not let you ${verb} ${r}.`}>
              <input type="checkbox" checked={value.includes(r)} disabled={!ok && !value.includes(r)} onChange={() => toggle(r)} />
              <span className="mono">{r}</span>
              {!ok && <span className="meta">not yours to {verb}</span>}
            </label>
          );
        })}
        {names.data && visible.length === 0 && <p className="meta">No repository matches.</p>}
      </div>
      <Field
        id={ids.pattern}
        label="Or a pattern"
        error={patternError}
        hint="* matches any characters: npm-* covers every repository starting with npm-, including future ones."
        addon={
          <button type="button" className="button button--secondary button--sm" onClick={addPattern}>
            Add pattern
          </button>
        }
      >
        <input
          className="input input--mono"
          value={pattern}
          placeholder="npm-*"
          autoComplete="off"
          spellCheck={false}
          onChange={(e) => setPattern(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'Enter') {
              e.preventDefault();
              addPattern();
            }
          }}
        />
      </Field>
    </div>
  );
}

/* ---------------------------------------------------------------- custom */

function CustomEditor({
  draft,
  setDraft,
  privileges,
  whose,
}: {
  draft: PermissionDraft;
  setDraft: (f: (d: PermissionDraft) => PermissionDraft) => void;
  privileges: string[] | undefined;
  whose: string;
}) {
  const toggleAction = (a: Action) =>
    setDraft((d) => ({ ...d, repoActions: d.repoActions.includes(a) ? d.repoActions.filter((x) => x !== a) : [...d.repoActions, a] }));
  return (
    <>
      <div className="chip-row" role="group" aria-label="Repository actions">
        <span className="field__label">In those repositories</span>
        {ACTIONS.map((a) => {
          const ok = !privileges || ownerHasRepoAction(privileges, a.id);
          return (
            <label key={a.id} className={ok ? 'checkbox' : 'checkbox is-disabled'} title={ok ? a.hint : `${whose} grant no ${a.label.toLowerCase()} access to any repository.`}>
              <input type="checkbox" checked={draft.repoActions.includes(a.id)} disabled={!ok} onChange={() => toggleAction(a.id)} />
              {a.label}
            </label>
          );
        })}
      </div>
      <div className="system-grid" role="group" aria-label="Administration areas">
        <span className="field__label">Administration</span>
        {SYSTEM_AREAS.map((a) => {
          const id = `sys-${a.id}`;
          const max: SystemLevel = privileges ? ownerLevel(privileges, a.id) : 'admin';
          const current = draft.system[a.id] ?? '';
          return (
            <div key={a.id} className="system-grid__row">
              <label htmlFor={id}>
                <span className="option__title">{a.label}</span>
                <span className="option__hint">{max === '' ? `${whose} grant no access here` : a.hint}</span>
              </label>
              <Select
                id={id}
                size="sm"
                value={current}
                options={SYSTEM_LEVELS.map((l) => ({
                  value: l.id,
                  label: l.label,
                  disabled: !levelAllowed(max, l.id),
                  hint: levelAllowed(max, l.id) ? undefined : `Beyond ${whose.toLowerCase()}`,
                }))}
                onChange={(v) => setDraft((d) => ({ ...d, system: { ...d.system, [a.id]: v } }))}
              />
            </div>
          );
        })}
      </div>
    </>
  );
}
