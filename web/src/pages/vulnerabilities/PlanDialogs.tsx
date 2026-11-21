import { useId, useState, type FormEvent, type ReactNode } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { revokeWaivers, useAssignees, useAssignFinding, useAssignPackage, useWaivePackage } from '../../api/findings';
import { useSession } from '../../api/hooks';
import { Dialog } from '../../components/Dialog';
import { errorText } from '../../lib/apiError';
import { toast } from '../../lib/toast';
import { dateInputValue, MAX_WAIVER_DAYS, shortDate, validateWaiver, waiverUntil } from '../../lib/vulnerabilities';

interface FormDialogProps {
  title: string;
  children: ReactNode;
  submitLabel: string;
  busy: boolean;
  error?: string | null;
  onSubmit: () => void;
  onCancel: () => void;
  /** More buttons before Cancel (e.g. "Remove assignee"). */
  extra?: ReactNode;
}

/** A form in the shared Dialog: it cannot be dismissed while it saves, and its buttons stay at the bottom edge. */
function FormDialog({ title, children, submitLabel, busy, error, onSubmit, onCancel, extra }: FormDialogProps) {
  const submit = (e: FormEvent) => {
    e.preventDefault();
    if (!busy) onSubmit();
  };
  return (
    <Dialog title={title} onClose={onCancel} dismissible={!busy} className="vdialog">
      <form className="dialog-form" onSubmit={submit} noValidate>
        {children}
        {error && (
          <p className="form-error" role="alert">
            {error}
          </p>
        )}
        <div className="dialog-actions">
          {extra}
          <button type="button" className="button button--secondary" onClick={onCancel} disabled={busy}>
            Cancel
          </button>
          <button type="submit" className="button button--primary" disabled={busy} aria-busy={busy || undefined}>
            {busy ? 'Saving…' : submitLabel}
          </button>
        </div>
      </form>
    </Dialog>
  );
}

export interface WaiveChoice {
  id: string;
  label: string;
}

interface WaiveDialogProps {
  /** Package key ("nuget:Newtonsoft.Json") and its name. */
  packageKey: string;
  packageName: string;
  /** The unresolved findings of the package that can be waived. */
  findings: WaiveChoice[];
  /** A finding picked up front; otherwise every finding is the default. */
  initial?: string;
  onClose: () => void;
}

/** Waive every finding of a package, or one of them, with a reason and an end date. */
export function WaiveDialog({ packageKey, packageName, findings, initial, onClose }: WaiveDialogProps) {
  const waive = useWaivePackage();
  const qc = useQueryClient();
  const reasonId = useId();
  const untilId = useId();
  const scopeName = useId();
  const [scope, setScope] = useState<string>(initial ?? 'all');
  const [reason, setReason] = useState('');
  const today = new Date();
  const [until, setUntil] = useState(() => dateInputValue(new Date(Date.now() + 30 * 86_400_000)));
  const [touched, setTouched] = useState(false);
  const errors = validateWaiver(reason, until);

  const submit = () => {
    setTouched(true);
    const end = waiverUntil(until);
    if (errors.reason || errors.until || !end) return;
    waive.mutate(
      { package: packageKey, finding_ids: scope === 'all' ? [] : [scope], reason: reason.trim(), until: end.toISOString() },
      {
        onSuccess: (res) => {
          onClose();
          const n = res.findings.length;
          toast(`Waived ${n === 1 ? '1 finding' : `${n} findings`} of ${packageName} until ${shortDate(end.toISOString())}. They are listed under Waived.`, {
            label: 'Undo',
            run: () =>
              void revokeWaivers(qc, res.findings).then(
                () => toast(`Waiver of ${packageName} undone.`),
                () => toast(`Could not undo the waiver of ${packageName}. End it from the Waived tab.`),
              ),
          });
        },
      },
    );
  };

  return (
    <FormDialog title={`Waive ${packageName}`} submitLabel="Waive" busy={waive.isPending} error={waive.isError ? errorText(waive.error, { action: 'waive these findings' }) : null} onSubmit={submit} onCancel={onClose}>
      {findings.length > 1 ? (
        <fieldset className="vchoice">
          <legend className="field__label">Findings</legend>
          <label className="vchoice__item">
            <input type="radio" name={scopeName} value="all" checked={scope === 'all'} onChange={() => setScope('all')} />
            <span>All {findings.length} findings of this package</span>
          </label>
          {findings.map((f) => (
            <label key={f.id} className="vchoice__item">
              <input type="radio" name={scopeName} value={f.id} checked={scope === f.id} onChange={() => setScope(f.id)} />
              <span className="vchoice__mono">{f.label}</span>
            </label>
          ))}
        </fieldset>
      ) : (
        findings[0] && <p className="meta">Waives {findings[0].label}.</p>
      )}
      <div className="field">
        <label htmlFor={reasonId} className="field__label">
          Reason
        </label>
        <textarea
          id={reasonId}
          className="input textarea"
          rows={3}
          value={reason}
          maxLength={2000}
          onChange={(e) => setReason(e.target.value)}
          aria-invalid={touched && errors.reason ? true : undefined}
          aria-describedby={`${reasonId}-msg`}
          placeholder="Why this does not affect us, or what mitigates it"
          data-autofocus
        />
        <span id={`${reasonId}-msg`} className={touched && errors.reason ? 'field__error' : 'field__hint'}>
          {touched && errors.reason ? errors.reason : 'Recorded in the audit log with your name.'}
        </span>
      </div>
      <div className="field">
        <label htmlFor={untilId} className="field__label">
          Waived until
        </label>
        <input
          id={untilId}
          className="input"
          type="date"
          value={until}
          min={dateInputValue(today)}
          max={dateInputValue(new Date(today.getTime() + MAX_WAIVER_DAYS * 86_400_000))}
          onChange={(e) => setUntil(e.target.value)}
          aria-invalid={touched && errors.until ? true : undefined}
          aria-describedby={`${untilId}-msg`}
        />
        <span id={`${untilId}-msg`} className={touched && errors.until ? 'field__error' : 'field__hint'}>
          {touched && errors.until ? errors.until : 'Ends at the end of that day (UTC), at most a year ahead. The findings re-open on their own then.'}
        </span>
      </div>
    </FormDialog>
  );
}

export type AssignTarget = { kind: 'finding'; id: string; label: string } | { kind: 'package'; key: string; label: string };

/** Pick the Hangar user responsible for a finding or a whole package. */
export function AssignDialog({ target, current, onClose }: { target: AssignTarget; current: string | null; onClose: () => void }) {
  const session = useSession();
  const me = session.data?.user.username;
  const [q, setQ] = useState('');
  const [picked, setPicked] = useState<string | null>(current);
  const users = useAssignees(q.trim(), true);
  const one = useAssignFinding();
  const pkg = useAssignPackage();
  const busy = one.isPending || pkg.isPending;
  const err = one.error ?? pkg.error;
  const searchId = useId();
  const listName = useId();

  const save = (assignee: string | null) => {
    const done = () => {
      onClose();
      toast(assignee ? `Assigned ${target.label} to ${assignee}.` : `${target.label} is no longer assigned.`);
    };
    if (target.kind === 'finding') one.mutate({ id: target.id, assignee }, { onSuccess: done });
    else pkg.mutate({ key: target.key, assignee }, { onSuccess: done });
  };
  const [missing, setMissing] = useState(false);
  const submit = () => {
    if (!picked) {
      setMissing(true);
      return;
    }
    save(picked);
  };
  const items = users.data?.items ?? [];

  return (
    <FormDialog
      title={`Assign ${target.label}`}
      submitLabel="Assign"
      busy={busy}
      error={err ? errorText(err, { action: 'assign it' }) : missing ? 'Pick a user.' : null}
      onSubmit={submit}
      onCancel={onClose}
      extra={
        current ? (
          <button type="button" className="button button--secondary" disabled={busy} onClick={() => save(null)}>
            Remove {current}
          </button>
        ) : undefined
      }
    >
      <p className="meta">
        {target.kind === 'package'
          ? 'Every open finding of the package, and new ones found later, go to this user.'
          : 'The user responsible for fixing this finding.'}{' '}
        Shown in the fix plan; no message is sent.
      </p>
      <div className="field">
        <label htmlFor={searchId} className="field__label">
          Find a user
        </label>
        <input id={searchId} className="input" type="search" value={q} onChange={(e) => setQ(e.target.value)} placeholder="Name or email" autoComplete="off" data-autofocus />
      </div>
      {me && (
        <button
          type="button"
          className="button button--secondary button--sm vassign__me"
          aria-pressed={picked === me}
          onClick={() => {
            setPicked(me);
            setMissing(false);
          }}
        >
          Assign to me ({me})
        </button>
      )}
      <fieldset className="vchoice vchoice--scroll" aria-busy={users.isFetching || undefined}>
        <legend className="visually-hidden">Users</legend>
        {users.isError ? (
          <p className="meta">{errorText(users.error, { action: 'list users' })}</p>
        ) : users.isPending ? (
          <p className="meta">Loading users…</p>
        ) : items.length === 0 ? (
          <p className="meta">No active user matches “{q}”.</p>
        ) : (
          items.map((u) => (
            <label key={u.username} className="vchoice__item">
              <input
                type="radio"
                name={listName}
                value={u.username}
                checked={picked === u.username}
                onChange={() => {
                  setPicked(u.username);
                  setMissing(false);
                }}
              />
              <span>
                <b>{u.username}</b>
                {u.display_name && u.display_name !== u.username && <span className="muted"> · {u.display_name}</span>}
              </span>
            </label>
          ))
        )}
      </fieldset>
    </FormDialog>
  );
}
