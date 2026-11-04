/** Form controls shared by the Settings pages. */
import { useId, useState, type ReactNode } from 'react';
import { IconClose, IconPlus } from '../../components/icons';
import { Pill } from '../../components/Pill';
import { Select } from '../../components/Select';
import type { GroupRole, TestResult } from './settingsApi';

/** Shown next to a value the configuration file sets. */
export function FileBadge() {
  return <Pill tone="neutral">set in configuration file</Pill>;
}

interface FieldProps {
  id: string;
  label: ReactNode;
  hint?: ReactNode;
  error?: string;
  optional?: boolean;
  wide?: boolean;
  children: (a11y: { id: string; 'aria-invalid'?: true; 'aria-describedby'?: string }) => ReactNode;
}

/** A labelled control with its hint or error linked by aria-describedby. */
export function Field({ id, label, hint, error, optional, wide, children }: FieldProps) {
  const msgId = `${id}-msg`;
  return (
    <div className={wide ? 'field field--wide' : 'field'}>
      <label htmlFor={id} className="field__label">
        {label} {optional && <span className="field__optional">optional</span>}
      </label>
      {children({ id, 'aria-invalid': error ? true : undefined, 'aria-describedby': error || hint ? msgId : undefined })}
      {error ? (
        <span id={msgId} className="field__error">
          {error}
        </span>
      ) : hint ? (
        <span id={msgId} className="field__hint">
          {hint}
        </span>
      ) : null}
    </div>
  );
}

/** A switch-styled checkbox for "enabled" toggles. */
export function Toggle({ checked, onChange, label, disabled }: { checked: boolean; onChange: (v: boolean) => void; label: ReactNode; disabled?: boolean }) {
  return (
    <label className="settings-toggle">
      <input type="checkbox" role="switch" checked={checked} disabled={disabled} onChange={(e) => onChange(e.target.checked)} />
      <span className="settings-toggle__track" aria-hidden="true" />
      <span className="settings-toggle__label">{label}</span>
    </label>
  );
}

/**
 * Edits a list of strings as chips: type and press Enter (or Add). Commas and
 * line breaks split pasted lists.
 */
export function ListInput({
  id,
  values,
  onChange,
  placeholder,
  disabled,
  invalid,
  describedBy,
  itemLabel,
}: {
  id: string;
  values: string[];
  onChange: (v: string[]) => void;
  placeholder?: string;
  disabled?: boolean;
  invalid?: boolean;
  describedBy?: string;
  /** Names one entry for the remove buttons ("Remove host x"). */
  itemLabel: string;
}) {
  const [draft, setDraft] = useState('');
  const add = () => {
    const items = draft.split(/[\n,]+/).map((s) => s.trim()).filter(Boolean);
    if (!items.length) return;
    onChange([...values, ...items.filter((s) => !values.includes(s))]);
    setDraft('');
  };
  return (
    <div className="list-input">
      {values.length > 0 && (
        <ul className="chips" aria-label={`${itemLabel}s`}>
          {values.map((v) => (
            <li key={v} className="chip">
              <span className="chip__text">{v}</span>
              {!disabled && (
                <button type="button" className="chip__remove" aria-label={`Remove ${itemLabel} ${v}`} onClick={() => onChange(values.filter((x) => x !== v))}>
                  <IconClose size={14} />
                </button>
              )}
            </li>
          ))}
        </ul>
      )}
      {!disabled && (
        <span className="input-with-button">
          <input
            id={id}
            className="input input--mono"
            value={draft}
            placeholder={placeholder}
            autoComplete="off"
            spellCheck={false}
            aria-invalid={invalid || undefined}
            aria-describedby={describedBy}
            onChange={(e) => setDraft(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === 'Enter') {
                e.preventDefault();
                add();
              }
            }}
            onBlur={add}
          />
          <button type="button" className="button button--secondary button--sm" onClick={add} disabled={!draft.trim()}>
            Add
          </button>
        </span>
      )}
      {disabled && values.length === 0 && <span className="meta">None</span>}
    </div>
  );
}

/**
 * A write-only secret: shows whether one is stored, and lets the user
 * replace or clear it. `value` is the new secret ("" keeps the stored one).
 */
export function SecretInput({
  id,
  configured,
  value,
  onChange,
  cleared,
  onClear,
  disabled,
  a11y,
}: {
  id: string;
  configured: boolean;
  value: string;
  onChange: (v: string) => void;
  cleared: boolean;
  onClear: (v: boolean) => void;
  disabled?: boolean;
  a11y: { 'aria-invalid'?: true; 'aria-describedby'?: string };
}) {
  if (cleared) {
    return (
      <span className="secret-state">
        <span className="meta">Removed when you save.</span>
        <button type="button" className="button button--secondary button--sm" onClick={() => onClear(false)} disabled={disabled}>
          Undo
        </button>
      </span>
    );
  }
  return (
    <span className="input-with-button">
      <input
        id={id}
        className="input"
        type="password"
        autoComplete="new-password"
        placeholder={configured ? 'Stored — leave empty to keep' : undefined}
        value={value}
        disabled={disabled}
        onChange={(e) => onChange(e.target.value)}
        {...a11y}
      />
      {configured && !disabled && (
        <button type="button" className="button button--secondary button--sm" onClick={() => onClear(true)}>
          Remove
        </button>
      )}
    </span>
  );
}

const STEP_TONE = { ok: 'ok', failed: 'bad', skipped: 'neutral' } as const;
const STEP_LABEL = { ok: 'OK', failed: 'Failed', skipped: 'Skipped' } as const;
const STEP_NAME: Record<string, string> = {
  discovery: 'Discovery',
  issuer: 'Issuer',
  keys: 'Signing keys',
  client_credentials: 'Client credentials',
  connect: 'Connect',
  bind: 'Service bind',
  lookup: 'User lookup',
  request: 'Request',
};

/** The outcome of a "Test" button, announced politely. */
export function TestResultView({ result }: { result: TestResult }) {
  return (
    <div className={result.ok ? 'test-result test-result--ok' : 'test-result test-result--bad'} role="status" aria-live="polite">
      <p className="test-result__summary">
        <strong>{result.ok ? 'Test passed' : 'Test failed'}</strong> · {result.message}
        {result.latency_ms > 0 && <span className="meta"> · {result.latency_ms} ms</span>}
      </p>
      <ol className="test-result__steps">
        {result.steps.map((s) => (
          <li key={s.name}>
            <Pill tone={STEP_TONE[s.status]}>{STEP_LABEL[s.status]}</Pill>
            <span className="test-result__name">{STEP_NAME[s.name] ?? s.name}</span>
            <span className="test-result__msg">{s.message}</span>
          </li>
        ))}
      </ol>
      {result.user && (
        <dl className="facts facts--compact test-result__user">
          <div>
            <dt>Entry</dt>
            <dd>
              <code>{result.user.dn}</code>
            </dd>
          </div>
          <div>
            <dt>Username · email</dt>
            <dd>
              {result.user.username}
              {result.user.email && ` · ${result.user.email}`}
            </dd>
          </div>
          <div>
            <dt>Groups ({result.user.groups.length})</dt>
            <dd>{result.user.groups.length ? result.user.groups.slice(0, 12).join(', ') + (result.user.groups.length > 12 ? ', …' : '') : 'None'}</dd>
          </div>
          <div>
            <dt>Roles granted by the mapping</dt>
            <dd>{result.user.roles.length ? result.user.roles.join(', ') : 'None'}</dd>
          </div>
        </dl>
      )}
    </div>
  );
}

/**
 * Several roles as chips, added with the shared Select (one role at a time)
 * and removed with each chip's button. Roles that no longer exist stay
 * visible so saving shows the server's error for them.
 */
export function RolesInput({
  id,
  value,
  onChange,
  roles,
  label,
  owner,
  disabled,
  invalid,
  describedBy,
}: {
  id: string;
  value: string[];
  onChange: (v: string[]) => void;
  roles: string[];
  /** Accessible name of the add control, e.g. "Add a default role". */
  label: string;
  /** What the roles belong to, for the remove buttons ("default roles", "/devops"). */
  owner: string;
  disabled?: boolean;
  invalid?: boolean;
  describedBy?: string;
}) {
  const options = roles
    .filter((r) => !value.includes(r))
    .sort((a, b) => a.localeCompare(b))
    .map((r) => ({ value: r, label: r }));
  return (
    <div className="list-input">
      {value.length > 0 && (
        <ul className="chips" aria-label="Chosen roles">
          {value.map((r) => (
            <li key={r} className="chip chip--role">
              <span className="chip__text">{r}</span>
              {!disabled && (
                <button type="button" className="chip__remove" aria-label={`Remove ${r} from ${owner}`} onClick={() => onChange(value.filter((x) => x !== r))}>
                  <IconClose size={14} />
                </button>
              )}
            </li>
          ))}
        </ul>
      )}
      {!disabled && (
        <Select
          id={id}
          value=""
          options={options}
          onChange={(r) => r && onChange([...value, r])}
          placeholder={options.length ? 'Add a role…' : roles.length ? 'Every role is chosen' : 'No roles to choose from'}
          disabled={options.length === 0}
          aria-label={label}
          aria-invalid={invalid || undefined}
          aria-describedby={describedBy}
        />
      )}
      {disabled && value.length === 0 && <span className="meta">None</span>}
    </div>
  );
}

/** Group → role mapping as a table: one row per group, roles picked from the existing roles. */
export function RoleMappingTable({
  rows,
  onChange,
  roles,
  errors,
  disabled,
  groupPlaceholder,
}: {
  rows: GroupRole[];
  onChange: (rows: GroupRole[]) => void;
  roles: string[];
  errors: Record<string, string>;
  disabled?: boolean;
  groupPlaceholder: string;
}) {
  const baseId = useId();
  const update = (i: number, row: GroupRole) => onChange(rows.map((r, j) => (j === i ? row : r)));
  return (
    <div className="mapping">
      {rows.length > 0 ? (
        <table className="mapping__table">
          <caption className="visually-hidden">Group to role mapping</caption>
          <thead>
            <tr>
              <th scope="col">Group</th>
              <th scope="col">Roles</th>
              <th scope="col">
                <span className="visually-hidden">Actions</span>
              </th>
            </tr>
          </thead>
          <tbody>
            {rows.map((r, i) => {
              const gErr = errors[`group_roles[${i}].group`];
              const rErr = errors[`group_roles[${i}].roles`];
              const gid = `${baseId}-g${i}`;
              const rid = `${baseId}-r${i}`;
              return (
                <tr key={i}>
                  <td data-label="Group">
                    <input
                      id={gid}
                      className="input input--mono"
                      value={r.group}
                      placeholder={groupPlaceholder}
                      disabled={disabled}
                      aria-label={`Group, row ${i + 1}`}
                      aria-invalid={gErr ? true : undefined}
                      aria-describedby={gErr ? `${gid}-err` : undefined}
                      onChange={(e) => update(i, { ...r, group: e.target.value })}
                    />
                    {gErr && (
                      <span id={`${gid}-err`} className="field__error">
                        {gErr}
                      </span>
                    )}
                  </td>
                  <td data-label="Roles">
                    <RolesInput
                      id={rid}
                      value={r.roles}
                      onChange={(v) => update(i, { ...r, roles: v })}
                      roles={roles}
                      label={`Add a role for ${r.group || `row ${i + 1}`}`}
                      owner={r.group || `row ${i + 1}`}
                      disabled={disabled}
                      invalid={!!rErr}
                      describedBy={rErr ? `${rid}-err` : undefined}
                    />
                    {rErr && (
                      <span id={`${rid}-err`} className="field__error">
                        {rErr}
                      </span>
                    )}
                  </td>
                  <td className="mapping__actions">
                    {!disabled && (
                      <button
                        type="button"
                        className="icon-button icon-button--sm"
                        aria-label={`Remove mapping ${r.group || `row ${i + 1}`}`}
                        onClick={() => onChange(rows.filter((_, j) => j !== i))}
                      >
                        <IconClose size={16} />
                      </button>
                    )}
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      ) : (
        <p className="meta">No group mappings. Users get the default roles only.</p>
      )}
      {!disabled && (
        <button type="button" className="button button--secondary button--sm mapping__add" onClick={() => onChange([...rows, { group: '', roles: [] }])}>
          <IconPlus size={16} /> Add mapping
        </button>
      )}
    </div>
  );
}
