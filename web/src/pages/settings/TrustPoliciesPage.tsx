import { useId, useState, type FormEvent } from 'react';
import { useSystemStatus } from '../../api/hooks';
import { useCanSystem } from '../../api/ops';
import { CodeBlock } from '../../components/CodeBlock';
import { Combobox } from '../../components/Combobox';
import { ConfirmDialog } from '../../components/ConfirmDialog';
import { DataTable, type Column } from '../../components/DataTable';
import { Dialog } from '../../components/Dialog';
import { ErrorNote } from '../../components/ErrorNote';
import { IconClose, IconPlus } from '../../components/icons';
import { PageHeader } from '../../components/PageHeader';
import { Panel } from '../../components/Panel';
import { Pill } from '../../components/Pill';
import { Select } from '../../components/Select';
import { EmptyState, ErrorMessage } from '../../components/States';
import { errorText } from '../../lib/apiError';
import { ScopeList } from '../access/tokens';
import { serverBaseUrl } from '../access/tokenSnippets';
import { Field, ListInput, Toggle } from './controls';
import {
  conditionText,
  issuerLabel,
  newPolicyForm,
  policyFieldErrors,
  policyForm,
  policyInput,
  presetFor,
  PRESETS,
  ttlText,
  usageSnippets,
  validatePolicy,
  type ConditionRow,
  type MatchKind,
  type PolicyErrors,
  type PolicyForm,
  type Preset,
} from './trustPolicyModel';
import {
  useCreateTrustPolicy,
  useDeleteTrustPolicy,
  useServiceAccountNames,
  useTrustPolicies,
  useUpdateTrustPolicy,
  type TrustPolicy,
} from './trustPoliciesApi';
import './settings.css';

type Editing = { mode: 'new' } | { mode: 'edit'; policy: TrustPolicy } | null;

/** The server's public URL, for the audience default and the snippets. */
function useBaseUrl(): string {
  const status = useSystemStatus();
  return serverBaseUrl(status.data?.base_url, window.location.origin);
}

/** Settings › CI trust policies: which CI jobs may trade their platform's ID token for a short-lived Hangar token. */
export function TrustPoliciesPage() {
  const canAdmin = useCanSystem('settings', 'admin');
  const q = useTrustPolicies();
  const baseUrl = useBaseUrl();
  const [editing, setEditing] = useState<Editing>(null);
  const [deleting, setDeleting] = useState<TrustPolicy | null>(null);
  const [usage, setUsage] = useState<TrustPolicy | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const del = useDeleteTrustPolicy();
  const items = q.data?.items ?? [];

  const startNew = () => {
    setNotice(null);
    setEditing({ mode: 'new' });
  };

  const columns: Column<TrustPolicy>[] = [
    {
      key: 'name',
      header: 'Name',
      cell: (p) => (
        <span className="trust-name">
          <strong>{p.name}</strong>
          {!p.enabled && <Pill tone="neutral">Disabled</Pill>}
          {p.description && <span className="meta">{p.description}</span>}
        </span>
      ),
    },
    {
      key: 'issuer',
      header: 'Issuer',
      cell: (p) => (
        <span className="trust-issuer">
          {issuerLabel(p.issuer)}
          {presetFor(p.issuer).id !== 'other' && <code>{p.issuer}</code>}
        </span>
      ),
    },
    {
      key: 'conditions',
      header: 'Conditions',
      priority: 2,
      cell: (p) => (
        <ul className="trust-conditions">
          {p.conditions.map((c, i) => (
            <li key={i}>
              <code>{conditionText(c)}</code>
            </li>
          ))}
        </ul>
      ),
    },
    {
      key: 'grants',
      header: 'Token',
      priority: 3,
      label: 'Token',
      cell: (p) => (
        <span className="trust-issuer">
          <span>
            Acts as <strong>{p.service_account}</strong> · {ttlText(p.max_ttl_seconds)}
          </span>
          <ScopeList scopes={p.scopes} />
        </span>
      ),
    },
    {
      key: 'actions',
      header: <span className="visually-hidden">Actions</span>,
      label: 'Actions',
      align: 'right',
      cell: (p) => (
        <span className="row-actions">
          <button type="button" className="button button--secondary button--sm" aria-label={`How to use ${p.name}`} onClick={() => setUsage(p)}>
            Usage
          </button>
          <button
            type="button"
            className="button button--secondary button--sm"
            aria-label={`${canAdmin ? 'Edit' : 'View'} ${p.name}`}
            onClick={() => {
              setNotice(null);
              setEditing({ mode: 'edit', policy: p });
            }}
          >
            {canAdmin ? 'Edit' : 'View'}
          </button>
          {canAdmin && (
            <button type="button" className="button button--ghost-danger button--sm" aria-label={`Delete ${p.name}`} onClick={() => setDeleting(p)}>
              Delete
            </button>
          )}
        </span>
      ),
    },
  ];

  return (
    <div className="screen settings-screen trust-screen">
      <PageHeader
        title="CI trust policies"
        description="Let CI jobs on GitHub Actions, GitLab and other OpenID Connect platforms publish with a short-lived token instead of a stored secret."
        actions={
          canAdmin &&
          items.length > 0 && (
            <button type="button" className="button button--primary" onClick={startNew}>
              <IconPlus size={16} /> Add policy
            </button>
          )
        }
      />
      <p className="visually-hidden" role="status">
        {notice ?? ''}
      </p>
      {notice && (
        <div className="notice">
          <span aria-hidden="true">{notice}</span>
          <button type="button" className="button button--secondary button--sm" onClick={() => setNotice(null)}>
            Dismiss
          </button>
        </div>
      )}

      {q.isError ? (
        <ErrorNote what="the trust policies" error={q.error} privilege="system:settings:read" onRetry={() => void q.refetch()} />
      ) : !q.isPending && items.length === 0 ? (
        <Panel>
          <EmptyState
            title="No trust policies yet"
            action={
              canAdmin ? (
                <button type="button" className="button button--primary" onClick={startNew}>
                  <IconPlus size={16} /> Add policy
                </button>
              ) : undefined
            }
          >
            {canAdmin
              ? 'A policy names the CI jobs it trusts (repository, branch, environment), the service account their tokens act as and what they may do.'
              : 'Adding a policy needs system:settings:admin.'}
          </EmptyState>
        </Panel>
      ) : (
        <Panel flush aria-label="CI trust policies">
          <DataTable caption="CI trust policies" columns={columns} rows={items} rowKey={(p) => p.id} loading={q.isPending} minWidth={860} />
        </Panel>
      )}

      {editing && (
        <PolicyDialog
          key={editing.mode === 'edit' ? editing.policy.id : 'new'}
          stored={editing.mode === 'edit' ? editing.policy : null}
          canAdmin={canAdmin}
          baseUrl={baseUrl}
          onClose={() => setEditing(null)}
          onSaved={(msg) => {
            setNotice(msg);
            setEditing(null);
          }}
        />
      )}

      {usage && <UsageDialog policy={usage} baseUrl={baseUrl} onClose={() => setUsage(null)} />}

      {deleting && (
        <ConfirmDialog
          title={`Delete trust policy ${deleting.name}?`}
          confirmLabel="Delete policy"
          tone="danger"
          busy={del.isPending}
          error={
            del.error
              ? errorText(del.error, { action: 'delete the policy', status: { 403: 'Deleting a policy needs system:settings:admin and every privilege of its scopes.' } })
              : null
          }
          onClose={() => {
            setDeleting(null);
            del.reset();
          }}
          onConfirm={() =>
            del.mutate(deleting.name, {
              onSuccess: () => {
                setNotice(`Trust policy ${deleting.name} deleted.`);
                setDeleting(null);
              },
            })
          }
        >
          <p>Jobs it trusted can no longer exchange their ID tokens, and the tokens it minted stop working now.</p>
        </ConfirmDialog>
      )}
    </div>
  );
}

/* ---------------------------------------------------------------- usage */

function UsageDialog({ policy, baseUrl, onClose }: { policy: TrustPolicy; baseUrl: string; onClose: () => void }) {
  const snippets = usageSnippets(policy, baseUrl);
  return (
    <Dialog title={`Use ${policy.name}`} description={`A job ${issuerLabel(policy.issuer)} signs for the audience ${policy.audience} gets a token of ${policy.service_account} for ${ttlText(policy.max_ttl_seconds)}.`} onClose={onClose} wide>
      <div className="trust-usage">
        {snippets.map((s) => (
          <section key={s.title} className="dialog-section" aria-label={s.title}>
            <h3 className="dialog-section__title">{s.title}</h3>
            <CodeBlock lines={s.lines} label={s.label} />
          </section>
        ))}
        <p className="meta">
          Each ID token can be exchanged once. Changing or deleting this policy revokes the tokens it minted.
        </p>
      </div>
    </Dialog>
  );
}

/* ---------------------------------------------------------------- editor */

const MATCH_OPTIONS: { value: MatchKind; label: string; hint: string }[] = [
  { value: 'exact', label: 'is', hint: 'Exactly one of the values' },
  { value: 'glob', label: 'matches', hint: '* any characters (/ too), ? one' },
];

interface DialogProps {
  stored: TrustPolicy | null;
  canAdmin: boolean;
  baseUrl: string;
  onClose: () => void;
  onSaved: (message: string) => void;
}

function PolicyDialog({ stored, canAdmin, baseUrl, onClose, onSaved }: DialogProps) {
  const id = useId();
  const [form, setForm] = useState<PolicyForm>(() => (stored ? policyForm(stored) : newPolicyForm()));
  const [errors, setErrors] = useState<PolicyErrors>({});
  const canListAccounts = useCanSystem('users', 'read');
  const accounts = useServiceAccountNames(canAdmin && canListAccounts);
  const create = useCreateTrustPolicy();
  const update = useUpdateTrustPolicy();
  const save = stored ? update : create;
  const locked = !canAdmin;
  const preset = presetFor(form.issuer);
  const set = <K extends keyof PolicyForm>(k: K, v: PolicyForm[K]) => setForm((f) => ({ ...f, [k]: v }));
  const setCondition = (i: number, row: ConditionRow) => set('conditions', form.conditions.map((c, j) => (j === i ? row : c)));
  const fieldsRefused = Object.keys(policyFieldErrors(save.error)).length > 0;

  const choosePreset = (p: Preset) => {
    setErrors({});
    setForm((f) => ({ ...f, issuer: p.issuer, conditions: p.conditions.map((c) => ({ ...c, values: [...c.values] })) }));
  };

  const submit = (e: FormEvent) => {
    e.preventDefault();
    const found = validatePolicy(form);
    setErrors(found);
    if (Object.keys(found).length) return;
    const body = policyInput(form);
    const onError = (err: unknown) => setErrors(policyFieldErrors(err));
    const done = (p: TrustPolicy) => onSaved(stored ? `Trust policy ${p.name} saved. Tokens it minted before are revoked.` : `Trust policy ${p.name} added.`);
    if (stored) update.mutate({ name: stored.name, body }, { onSuccess: done, onError });
    else create.mutate(body, { onSuccess: done, onError });
  };

  return (
    <Dialog
      title={stored ? (locked ? `Trust policy ${stored.name}` : `Edit ${stored.name}`) : 'New trust policy'}
      description={stored && !locked ? 'Saving revokes the tokens this policy minted before.' : undefined}
      onClose={onClose}
      dismissible={!save.isPending}
      wide
    >
      <form className="dialog-form" onSubmit={submit} noValidate>
        <fieldset className="settings-fieldset trust-form" disabled={locked}>
          <legend className="visually-hidden">Trust policy settings</legend>
          {!stored && (
            <div className="field">
              <span className="field__label" id={`${id}-preset`}>
                CI platform
              </span>
              <div className="trust-presets" role="group" aria-labelledby={`${id}-preset`}>
                {PRESETS.map((p) => (
                  <button
                    key={p.id}
                    type="button"
                    className={preset.id === p.id ? 'choice-chip is-on' : 'choice-chip'}
                    aria-pressed={preset.id === p.id}
                    onClick={() => choosePreset(p)}
                  >
                    {p.label}
                  </button>
                ))}
              </div>
            </div>
          )}

          <div className="form-grid">
            <Field id={`${id}-name`} label="Name" error={errors.name} hint={stored ? 'Cannot be changed' : 'Used in the exchange request when several policies match'}>
              {(a) => (
                <input
                  {...a}
                  className="input input--mono"
                  autoComplete="off"
                  spellCheck={false}
                  placeholder="acme-app-github"
                  value={form.name}
                  readOnly={!!stored}
                  onChange={(e) => set('name', e.target.value)}
                  data-autofocus={stored ? undefined : true}
                />
              )}
            </Field>
            <Field id={`${id}-desc`} label="Description" optional error={errors.description}>
              {(a) => <input {...a} className="input" autoComplete="off" value={form.description} onChange={(e) => set('description', e.target.value)} />}
            </Field>
          </div>

          <div className="form-grid">
            <Field id={`${id}-issuer`} label="Issuer" error={errors.issuer} hint="The exact iss claim of the ID tokens, an https URL">
              {(a) => (
                <input
                  {...a}
                  className="input input--mono"
                  type="url"
                  autoComplete="off"
                  spellCheck={false}
                  placeholder="https://gitlab.example.com"
                  value={form.issuer}
                  onChange={(e) => set('issuer', e.target.value)}
                />
              )}
            </Field>
            <Field id={`${id}-aud`} label="Audience" optional error={errors.audience} hint={`Must be in the token's aud. Empty: ${baseUrl}`}>
              {(a) => (
                <input {...a} className="input input--mono" autoComplete="off" spellCheck={false} placeholder={baseUrl} value={form.audience} onChange={(e) => set('audience', e.target.value)} />
              )}
            </Field>
          </div>

          <fieldset className="fieldset" aria-describedby={errors.conditions ? `${id}-cond-msg` : `${id}-cond-hint`}>
            <legend className="field__label">Conditions</legend>
            <span id={`${id}-cond-hint`} className="field__hint">
              Every condition must match a claim of the ID token.
              {preset.ownerClaims.length > 0 && ` One must fix the owner: ${preset.ownerClaims.join(', ')}, with exact values or globs such as acme/*.`}
            </span>
            <div className="conditions">
              {form.conditions.map((c, i) => (
                <ConditionEditor
                  key={i}
                  index={i}
                  row={c}
                  claims={preset.claims}
                  example={preset.examples[c.claim.trim()] ?? 'value'}
                  error={errors[`conditions[${i}]`]}
                  locked={locked}
                  onChange={(row) => setCondition(i, row)}
                  onRemove={() => set('conditions', form.conditions.filter((_, j) => j !== i))}
                />
              ))}
            </div>
            {errors.conditions && (
              <span id={`${id}-cond-msg`} className="field__error">
                {errors.conditions}
              </span>
            )}
            {!locked && form.conditions.length < 32 && (
              <button type="button" className="button button--secondary button--sm conditions__add" onClick={() => set('conditions', [...form.conditions, { claim: '', match: 'exact', values: [] }])}>
                <IconPlus size={16} /> Add condition
              </button>
            )}
          </fieldset>

          <div className="form-grid">
            <Field id={`${id}-sa`} label="Service account" error={errors.service_account} hint="The enabled service account the minted tokens act as">
              {(a) => (
                <Combobox
                  {...a}
                  className="input input--mono"
                  autoComplete="off"
                  spellCheck={false}
                  placeholder="ci-publisher"
                  value={form.serviceAccount}
                  options={accounts}
                  onChange={(v) => set('serviceAccount', v)}
                />
              )}
            </Field>
            <Field id={`${id}-ttl`} label="Token lifetime (minutes)" error={errors.ttl} hint="1 to 60; 15 by default">
              {(a) => (
                <input {...a} className="input input--mono" type="number" inputMode="numeric" min={1} max={60} step={1} value={form.ttlMinutes} onChange={(e) => set('ttlMinutes', e.target.value)} />
              )}
            </Field>
          </div>

          <Field
            id={`${id}-scopes`}
            label="Scopes"
            error={errors.scopes}
            hint="What the minted token may do, such as repo:npm-hosted:write. You can only hand out privileges you hold yourself"
          >
            {(a) => (
              <ListInput
                id={a.id}
                describedBy={a['aria-describedby']}
                invalid={!!a['aria-invalid']}
                itemLabel="scope"
                values={form.scopes}
                onChange={(v) => set('scopes', v)}
                disabled={locked}
                placeholder="repo:npm-hosted:write"
              />
            )}
          </Field>

          <Toggle checked={form.enabled} onChange={(v) => set('enabled', v)} label="Enabled" disabled={locked} />
        </fieldset>

        {save.isError && (
          <ErrorMessage
            error={save.error}
            message={
              fieldsRefused
                ? 'Fix the highlighted fields and save again.'
                : errorText(save.error, {
                    action: 'save the policy',
                    status: {
                      403: 'You can only hand out privileges you hold yourself: remove the scopes you do not have, or ask an administrator who holds them.',
                      409: 'A trust policy with this name already exists.',
                    },
                  })
            }
          />
        )}

        <div className="dialog-actions">
          <button type="button" className="button button--secondary" onClick={onClose} disabled={save.isPending}>
            {locked ? 'Close' : 'Cancel'}
          </button>
          {!locked && (
            <button type="submit" className="button button--primary" disabled={save.isPending} aria-busy={save.isPending || undefined}>
              {save.isPending ? 'Saving…' : stored ? 'Save' : 'Add policy'}
            </button>
          )}
        </div>
      </form>
    </Dialog>
  );
}

interface ConditionProps {
  index: number;
  row: ConditionRow;
  claims: string[];
  example: string;
  error?: string;
  locked: boolean;
  onChange: (row: ConditionRow) => void;
  onRemove: () => void;
}

function ConditionEditor({ index, row, claims, example, error, locked, onChange, onRemove }: ConditionProps) {
  const id = useId();
  const n = index + 1;
  const errId = `${id}-err`;
  const described = error ? errId : undefined;
  return (
    <div className="condition" role="group" aria-label={`Condition ${n}`}>
      <Combobox
        className="input input--mono"
        aria-label={`Claim of condition ${n}`}
        aria-invalid={error ? true : undefined}
        aria-describedby={described}
        autoComplete="off"
        spellCheck={false}
        placeholder="claim"
        value={row.claim}
        options={claims}
        disabled={locked}
        onChange={(claim) => onChange({ ...row, claim })}
      />
      <Select
        value={row.match}
        options={MATCH_OPTIONS}
        onChange={(match) => onChange({ ...row, match })}
        disabled={locked}
        aria-label={`Match of condition ${n}`}
      />
      <div>
        <label htmlFor={`${id}-values`} className="visually-hidden">
          Values of condition {n}
        </label>
        <ListInput
          id={`${id}-values`}
          values={row.values}
          onChange={(values) => onChange({ ...row, values })}
          itemLabel="value"
          placeholder={example}
          disabled={locked}
          invalid={!!error}
          describedBy={described}
        />
      </div>
      {!locked ? (
        <button type="button" className="icon-button icon-button--sm" aria-label={`Remove condition ${n}`} onClick={onRemove}>
          <IconClose size={16} />
        </button>
      ) : (
        <span />
      )}
      {error && (
        <span id={errId} className="field__error">
          {error}
        </span>
      )}
    </div>
  );
}
