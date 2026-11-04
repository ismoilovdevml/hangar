import { useId, useMemo, useState, type FormEvent } from 'react';
import { useSession } from '../../api/hooks';
import { CopyButton } from '../../components/CopyButton';
import { DataTable, type Column } from '../../components/DataTable';
import { Pill } from '../../components/Pill';
import { Select } from '../../components/Select';
import { relativeTime } from '../../lib/format';
import { useNow } from '../../lib/useNow';
import { useCreateToken, useRepositoryNames, useRevokeToken, type Token, type TokenCreated } from './api';
import {
  ACTIONS,
  buildScopes,
  customExpiry,
  describePrivilege,
  EMPTY_SCOPES,
  expiresInDays,
  expiryPresets,
  expiryText,
  formatDate,
  parseGoDuration,
  SYSTEM_AREAS,
  TOKEN_STATE_META,
  tokenState,
  uncoveredScopes,
  type Action,
  type RepoScopeMode,
  type ScopeDraft,
} from './model';
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

/** Tokens with prefix, scopes, created, last use (time and address), expiry and Revoke. */
export function TokenTable({ caption, tokens, showOwner, busy, empty }: TokenTableProps) {
  const now = useNow();
  const session = useSession();
  const warnMs = parseGoDuration(session.data?.token_policy?.expiry_warning) ?? undefined;
  const [revoking, setRevoking] = useState<Token | null>(null);
  const revoke = useRevokeToken();

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
    { key: 'scopes', header: 'Scopes', cell: (t) => <ScopeList scopes={t.scopes} /> },
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
      key: 'revoke',
      header: <span className="visually-hidden">Actions</span>,
      align: 'right',
      className: 'col-last',
      cell: (t) => (
        <button type="button" className="button button--secondary button--sm" onClick={() => setRevoking(t)} aria-label={`Revoke token ${t.name}`}>
          Revoke
        </button>
      ),
    },
  ];

  return (
    <>
      <DataTable caption={caption} columns={columns} rows={tokens} rowKey={(t) => t.id} busy={busy} minWidth={showOwner ? 960 : 820} empty={empty ?? 'No tokens.'} />
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
    </>
  );
}

/* ---------------------------------------------------------------- create */

const REPO_MODES: { id: RepoScopeMode; label: string; hint: string }[] = [
  { id: 'all', label: 'All repositories', hint: 'Including repositories created later' },
  { id: 'pattern', label: 'Matching a pattern', hint: 'For example npm-* or docker-prod' },
  { id: 'list', label: 'Selected repositories', hint: 'Pick from the list' },
  { id: 'none', label: 'No repository access', hint: 'Administration only' },
];

const SYSTEM_LEVELS = ['', 'read', 'write', 'delete', 'admin'] as const;

interface CreateTokenProps {
  /** A service account to create the token for; the caller when absent. */
  owner?: string;
  onClose: () => void;
}

export function CreateTokenDialog({ owner, onClose }: CreateTokenProps) {
  const [created, setCreated] = useState<TokenCreated | null>(null);
  return (
    <Dialog
      title={created ? 'Copy your token now' : owner ? `New token for ${owner}` : 'New personal access token'}
      description={
        created
          ? 'This is the only time the secret is shown. Hangar stores only a hash of it.'
          : 'Tokens authenticate CLIs and CI jobs. A token never grants more than its owner’s roles.'
      }
      onClose={onClose}
      wide
    >
      {created ? <SecretPanel token={created} onDone={onClose} /> : <TokenForm owner={owner} onCreated={setCreated} onCancel={onClose} />}
    </Dialog>
  );
}

function SecretPanel({ token, onDone }: { token: TokenCreated; onDone: () => void }) {
  const [stored, setStored] = useState(false);
  const host = typeof window !== 'undefined' ? window.location.host : 'hangar.example.com';
  return (
    <div className="dialog-form">
      <div className="secret-box">
        <code className="secret-box__value" data-testid="token-secret">
          {token.secret}
        </code>
        <CopyButton text={token.secret} label="Copy token" />
      </div>
      <dl className="summary__list">
        <dt>Name</dt>
        <dd>{token.name}</dd>
        <dt>Owner</dt>
        <dd>{token.owner}</dd>
        <dt>Expires</dt>
        <dd>{formatDate(token.expires_at)}</dd>
        <dt>Scopes</dt>
        <dd>
          <ScopeList scopes={token.scopes} max={10} />
        </dd>
      </dl>
      <div className="hint-box">
        <p className="field__label">Use it</p>
        <p className="meta">
          As the password with any username: <code className="inline-code">docker login {host} -u {token.owner} --password-stdin</code>, npm, Maven,
          pip and NuGet. Or as a header: <code className="inline-code">Authorization: Bearer hgr_…</code>
        </p>
      </div>
      <label className="checkbox">
        <input type="checkbox" checked={stored} onChange={(e) => setStored(e.target.checked)} />I have stored the token somewhere safe
      </label>
      <div className="dialog-actions">
        <button type="button" className="button button--primary" disabled={!stored} onClick={onDone}>
          Done
        </button>
      </div>
    </div>
  );
}

function TokenForm({ owner, onCreated, onCancel }: { owner?: string; onCreated: (t: TokenCreated) => void; onCancel: () => void }) {
  const session = useSession();
  const maxDays = session.data?.token_policy?.max_lifetime_days ?? 365;
  const presets = expiryPresets(maxDays);
  const [name, setName] = useState('');
  const [expiry, setExpiry] = useState<number | 'custom'>(presets.includes(30) ? 30 : (presets[0] ?? 30));
  const [customDate, setCustomDate] = useState('');
  const [draft, setDraft] = useState<ScopeDraft>(EMPTY_SCOPES);
  const [systemAll, setSystemAll] = useState(false);
  const [submitted, setSubmitted] = useState(false);
  const create = useCreateToken();
  const repoNames = useRepositoryNames(draft.repoMode === 'list');
  const [repoFilter, setRepoFilter] = useState('');
  const ids = { name: useId(), pattern: useId(), date: useId(), repoFilter: useId() };

  const effective: ScopeDraft = { ...draft, system: systemAll ? ['*:*'] : draft.system };
  const { scopes, error: scopeError } = buildScopes(effective);
  const custom = expiry === 'custom' ? customExpiry(customDate, maxDays) : null;
  const nameError = !name.trim() ? 'Give the token a name you will recognise.' : name.trim().length > 100 ? 'At most 100 characters.' : null;
  const uncovered = owner ? [] : uncoveredScopes(scopes, session.data?.privileges);

  const setSystem = (area: string, level: string) => {
    setDraft((d) => ({ ...d, system: [...d.system.filter((s) => !s.startsWith(`${area}:`)), ...(level ? [`${area}:${level}`] : [])] }));
  };
  const toggleAction = (a: Action) =>
    setDraft((d) => ({ ...d, actions: d.actions.includes(a) ? d.actions.filter((x) => x !== a) : [...d.actions, a] }));
  const toggleRepo = (r: string) =>
    setDraft((d) => ({ ...d, repos: d.repos.includes(r) ? d.repos.filter((x) => x !== r) : [...d.repos, r] }));

  const visibleRepos = useMemo(
    () => (repoNames.data ?? []).filter((r) => !repoFilter || r.includes(repoFilter.trim().toLowerCase())),
    [repoNames.data, repoFilter],
  );

  const submit = (e: FormEvent) => {
    e.preventDefault();
    setSubmitted(true);
    if (nameError || scopeError || (custom && custom.error)) return;
    const expiresAt = expiry === 'custom' ? custom!.value! : expiresInDays(expiry);
    create.mutate({ name: name.trim(), owner, scopes, expires_at: expiresAt }, { onSuccess: onCreated });
  };

  return (
    <form className="dialog-form" onSubmit={submit} noValidate>
      <Field id={ids.name} label="Name" error={submitted && nameError}>
        <input
          className="input"
          value={name}
          onChange={(e) => setName(e.target.value)}
          placeholder={owner ? 'e.g. gitlab-runner deploy' : 'e.g. laptop CLI'}
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
            On {formatDate(expiresInDays(expiry))}. Tokens always expire; the server allows at most {maxDays} days.
          </span>
        )}
      </fieldset>

      <fieldset className="fieldset">
        <legend className="field__label">Repository access</legend>
        <div className="option-grid">
          {REPO_MODES.map((m) => (
            <label key={m.id} className={draft.repoMode === m.id ? 'option is-on' : 'option'}>
              <input type="radio" name="repo-mode" checked={draft.repoMode === m.id} onChange={() => setDraft((d) => ({ ...d, repoMode: m.id }))} />
              <span>
                <span className="option__title">{m.label}</span>
                <span className="option__hint">{m.hint}</span>
              </span>
            </label>
          ))}
        </div>
        {draft.repoMode === 'pattern' && (
          <Field id={ids.pattern} label="Pattern" hint="* matches any characters, ? one character.">
            <input
              className="input input--mono"
              value={draft.pattern}
              onChange={(e) => setDraft((d) => ({ ...d, pattern: e.target.value }))}
              placeholder="npm-*"
              autoComplete="off"
              spellCheck={false}
            />
          </Field>
        )}
        {draft.repoMode === 'list' && (
          <div className="repo-picker">
            <label className="visually-hidden" htmlFor={ids.repoFilter}>
              Filter repositories
            </label>
            <input id={ids.repoFilter} className="input" placeholder="Filter repositories" value={repoFilter} onChange={(e) => setRepoFilter(e.target.value)} />
            <div className="repo-picker__list" role="group" aria-label="Repositories">
              {repoNames.isPending && <p className="meta">Loading repositories…</p>}
              {repoNames.isError && <p className="meta">Could not load repositories.</p>}
              {visibleRepos.map((r) => (
                <label key={r} className="checkbox">
                  <input type="checkbox" checked={draft.repos.includes(r)} onChange={() => toggleRepo(r)} />
                  <span className="mono">{r}</span>
                </label>
              ))}
              {repoNames.data && visibleRepos.length === 0 && <p className="meta">No repository matches.</p>}
            </div>
            <span className="field__hint">{draft.repos.length} selected</span>
          </div>
        )}
        {draft.repoMode !== 'none' && (
          <div className="chip-row" role="group" aria-label="Repository actions">
            {ACTIONS.map((a) => (
              <label key={a.id} className="checkbox" title={a.hint}>
                <input type="checkbox" checked={draft.actions.includes(a.id)} onChange={() => toggleAction(a.id)} />
                {a.label}
              </label>
            ))}
          </div>
        )}
      </fieldset>

      <details className="advanced" open={draft.system.length > 0 || systemAll}>
        <summary className="advanced__summary">Administration scopes (optional)</summary>
        <label className="checkbox">
          <input type="checkbox" checked={systemAll} onChange={(e) => setSystemAll(e.target.checked)} />
          Everything (<code className="inline-code">system:*</code>)
        </label>
        {!systemAll && (
          <div className="system-grid">
            {SYSTEM_AREAS.map((a) => {
              const current = draft.system.find((s) => s.startsWith(`${a.id}:`))?.split(':')[1] ?? '';
              const id = `sys-${a.id}`;
              return (
                <label key={a.id} className="system-grid__row" htmlFor={id}>
                  <span>
                    <span className="option__title">{a.label}</span>
                    <span className="option__hint">{a.hint}</span>
                  </span>
                  <Select
                    id={id}
                    size="sm"
                    value={current}
                    options={SYSTEM_LEVELS.map((l) => ({ value: l, label: l ? l.charAt(0).toUpperCase() + l.slice(1) : 'None' }))}
                    onChange={(v) => setSystem(a.id, v)}
                  />
                </label>
              );
            })}
          </div>
        )}
      </details>

      <div className="scope-preview" aria-live="polite">
        <p className="field__label">This token can</p>
        {scopeError ? (
          <p className={submitted ? 'field__error' : 'meta'}>{scopeError}</p>
        ) : (
          <ul className="scope-preview__list">
            {scopes.map((s) => (
              <li key={s}>
                {describePrivilege(s)} <code className="meta mono">{s}</code>
              </li>
            ))}
          </ul>
        )}
        {uncovered.length > 0 && (
          <p className="warn-note">
            Your roles do not grant {uncovered.join(', ')}; those scopes will have no effect unless your roles change.
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
