import { useId, useState, type FormEvent } from 'react';
import { useCanSystem } from '../../api/ops';
import { ConfirmDialog } from '../../components/ConfirmDialog';
import { DataTable, type Column } from '../../components/DataTable';
import { Dialog } from '../../components/Dialog';
import { ErrorNote } from '../../components/ErrorNote';
import { IconPlus } from '../../components/icons';
import { PageHeader } from '../../components/PageHeader';
import { Panel } from '../../components/Panel';
import { Pill } from '../../components/Pill';
import { Select } from '../../components/Select';
import { EmptyState, ErrorMessage } from '../../components/States';
import { Time } from '../../components/Time';
import { errorText } from '../../lib/apiError';
import { Field, ListInput, Toggle } from './controls';
import { WebhookDeliveries } from './WebhookDeliveries';
import {
  deliveryOutcome,
  eventLabel,
  formatLabel,
  NEW_WEBHOOK,
  secretMustBeRenewed,
  statusCodeTone,
  validateWebhook,
  WEBHOOK_EVENTS,
  WEBHOOK_FORMATS,
  webhookForm,
  webhookInput,
  webhookFieldErrors,
  webhookStatus,
  type SecretChoice,
  type WebhookErrors,
  type WebhookForm,
} from './webhookModel';
import { useCreateWebhook, useDeleteWebhook, useTestWebhook, useUpdateWebhook, useWebhooks, type Webhook, type WebhookDelivery } from './webhooksApi';
import './settings.css';

type Editing = { mode: 'new' } | { mode: 'edit'; hook: Webhook } | null;

/** Settings › Webhooks: who is told about published, deleted and quarantined packages, and how that went. */
export function WebhooksPage() {
  const canAdmin = useCanSystem('webhooks', 'admin');
  const q = useWebhooks();
  const [editing, setEditing] = useState<Editing>(null);
  const [deleting, setDeleting] = useState<Webhook | null>(null);
  const [deliveriesOf, setDeliveriesOf] = useState<Webhook | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const del = useDeleteWebhook();
  const items = q.data?.items ?? [];

  const startNew = () => {
    setNotice(null);
    setEditing({ mode: 'new' });
  };

  return (
    <div className="screen settings-screen webhooks-screen">
      <PageHeader
        title="Webhooks"
        description="Tell chat rooms, CI systems and your own services when packages are published, deleted or quarantined."
        actions={
          canAdmin &&
          items.length > 0 && (
            <button type="button" className="button button--primary" onClick={startNew}>
              <IconPlus size={16} /> Add webhook
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
        <ErrorNote what="the webhooks" error={q.error} privilege="system:webhooks:read" onRetry={() => void q.refetch()} />
      ) : (
        <WebhookList
          items={items}
          loading={q.isPending}
          canAdmin={canAdmin}
          onAdd={startNew}
          onEdit={(hook) => {
            setNotice(null);
            setEditing({ mode: 'edit', hook });
          }}
          onDelete={setDeleting}
          onDeliveries={setDeliveriesOf}
        />
      )}

      {editing && (
        <WebhookDialog
          key={editing.mode === 'edit' ? editing.hook.id : 'new'}
          stored={editing.mode === 'edit' ? editing.hook : null}
          canAdmin={canAdmin}
          onClose={() => setEditing(null)}
          onSaved={(msg) => {
            setNotice(msg);
            setEditing(null);
          }}
        />
      )}

      {deliveriesOf && <WebhookDeliveries hook={deliveriesOf} canAdmin={canAdmin} onClose={() => setDeliveriesOf(null)} />}

      {deleting && (
        <ConfirmDialog
          title={`Delete webhook ${deleting.name}?`}
          confirmLabel="Delete webhook"
          tone="danger"
          busy={del.isPending}
          error={del.error ? errorText(del.error, { action: 'delete the webhook' }) : null}
          onClose={() => {
            setDeleting(null);
            del.reset();
          }}
          onConfirm={() =>
            del.mutate(deleting.id, {
              onSuccess: () => {
                setNotice(`Webhook ${deleting.name} deleted.`);
                setDeleting(null);
              },
            })
          }
        >
          <p>Events stop going to {deleting.url}, and its delivery history is deleted with it.</p>
        </ConfirmDialog>
      )}
    </div>
  );
}

/* ---------------------------------------------------------------- list */

interface ListProps {
  items: Webhook[];
  loading: boolean;
  canAdmin: boolean;
  onAdd: () => void;
  onEdit: (h: Webhook) => void;
  onDelete: (h: Webhook) => void;
  onDeliveries: (h: Webhook) => void;
}

function WebhookList({ items, loading, canAdmin, onAdd, onEdit, onDelete, onDeliveries }: ListProps) {
  const test = useTestWebhook();
  const [tested, setTested] = useState<{ hook: Webhook; result: WebhookDelivery } | { hook: Webhook; error: unknown } | null>(null);
  const runTest = (h: Webhook) => {
    setTested(null);
    test.mutate(h.id, { onSuccess: (result) => setTested({ hook: h, result }), onError: (error) => setTested({ hook: h, error }) });
  };

  const columns: Column<Webhook>[] = [
    {
      key: 'name',
      header: 'Name',
      cell: (h) => {
        const status = webhookStatus(h);
        return (
          <span className="webhook-name">
            <strong>{h.name}</strong>
            <Pill tone={status.tone}>{status.label}</Pill>
            {h.disabled_reason && <span className="webhook-name__reason">{h.disabled_reason}</span>}
          </span>
        );
      },
    },
    {
      key: 'url',
      header: 'Sends to',
      cell: (h) => (
        <span className="webhook-url">
          <code>{h.url}</code>
          <span className="meta">
            {formatLabel(h.format)}
            {h.secret_configured ? ' · signed' : ''}
          </span>
        </span>
      ),
    },
    {
      key: 'events',
      header: 'Events',
      priority: 2,
      cell: (h) => (
        <span className="webhook-events">
          {h.events.map(eventLabel).join(', ')}
          <span className="meta">{h.repositories.length ? `Repositories: ${h.repositories.join(', ')}` : 'Every repository'}</span>
        </span>
      ),
    },
    {
      key: 'last',
      header: 'Last delivery',
      priority: 3,
      cell: (h) =>
        h.last_delivery_at === null ? (
          <span className="meta">None yet</span>
        ) : (
          <span className="webhook-last">
            {h.last_status_code !== null ? <Pill tone={statusCodeTone(h.last_status_code)}>HTTP {h.last_status_code}</Pill> : <Pill tone="bad">No answer</Pill>}
            <Time value={h.last_delivery_at} mode="relative" className="meta" />
            {h.consecutive_failures > 0 && <span className="meta">{h.consecutive_failures === 1 ? '1 failure' : `${h.consecutive_failures} failures`} in a row</span>}
          </span>
        ),
    },
    {
      key: 'actions',
      header: <span className="visually-hidden">Actions</span>,
      label: 'Actions',
      align: 'right',
      cell: (h) => (
        <span className="row-actions">
          <button type="button" className="button button--secondary button--sm" aria-label={`Deliveries of ${h.name}`} onClick={() => onDeliveries(h)}>
            Deliveries
          </button>
          {canAdmin && (
            <button type="button" className="button button--secondary button--sm" aria-label={`Test ${h.name}`} onClick={() => runTest(h)} disabled={test.isPending}>
              {test.isPending && test.variables === h.id ? 'Sending…' : 'Test'}
            </button>
          )}
          <button type="button" className="button button--secondary button--sm" aria-label={`${canAdmin ? 'Edit' : 'View'} ${h.name}`} onClick={() => onEdit(h)}>
            {canAdmin ? 'Edit' : 'View'}
          </button>
          {canAdmin && (
            <button type="button" className="button button--ghost-danger button--sm" aria-label={`Delete ${h.name}`} onClick={() => onDelete(h)}>
              Delete
            </button>
          )}
        </span>
      ),
    },
  ];

  if (!loading && items.length === 0) {
    return (
      <Panel>
        <EmptyState
          title="No webhooks yet"
          action={
            canAdmin ? (
              <button type="button" className="button button--primary" onClick={onAdd}>
                <IconPlus size={16} /> Add webhook
              </button>
            ) : undefined
          }
        >
          {canAdmin
            ? 'Send events as JSON to your own service, or as messages to a Slack or Microsoft Teams channel.'
            : 'Adding a webhook needs system:webhooks:admin.'}
        </EmptyState>
      </Panel>
    );
  }

  return (
    <Panel flush aria-label="Webhooks">
      {tested && (
        <div className="webhook-test" role="status" aria-live="polite">
          {'result' in tested ? (
            <p className={tested.result.state === 'succeeded' ? 'form-success' : 'form-error'}>
              Test of {tested.hook.name}: {tested.result.state === 'succeeded' ? 'delivered' : 'failed'} · {deliveryOutcome(tested.result)}
            </p>
          ) : (
            <ErrorMessage error={tested.error} message={`Could not test ${tested.hook.name}. ${errorText(tested.error, { action: 'test the webhook' })}`} />
          )}
        </div>
      )}
      <DataTable caption="Webhooks" columns={columns} rows={items} rowKey={(h) => h.id} loading={loading} minWidth={820} />
    </Panel>
  );
}

/* ---------------------------------------------------------------- editor */

const SECRET_CHOICES: { value: SecretChoice; title: string; hint: string }[] = [
  { value: 'keep', title: 'Keep the stored secret', hint: 'Deliveries stay signed with it' },
  { value: 'replace', title: 'Replace it', hint: 'Update the receiver at the same time' },
  { value: 'clear', title: 'Remove it', hint: 'Deliveries are no longer signed' },
];

interface DialogProps {
  stored: Webhook | null;
  canAdmin: boolean;
  onClose: () => void;
  onSaved: (message: string) => void;
}

function WebhookDialog({ stored, canAdmin, onClose, onSaved }: DialogProps) {
  const id = useId();
  const [form, setForm] = useState<WebhookForm>(() => (stored ? webhookForm(stored) : NEW_WEBHOOK));
  const [errors, setErrors] = useState<WebhookErrors>({});
  const create = useCreateWebhook();
  const update = useUpdateWebhook();
  const save = stored ? update : create;
  const locked = !canAdmin;
  const set = <K extends keyof WebhookForm>(k: K, v: WebhookForm[K]) => setForm((f) => ({ ...f, [k]: v }));
  const renew = secretMustBeRenewed(form, stored);
  const fieldsRefused = Object.keys(webhookFieldErrors(save.error)).length > 0;

  const submit = (e: FormEvent) => {
    e.preventDefault();
    const found = validateWebhook(form, stored);
    setErrors(found);
    if (Object.keys(found).length) return;
    const body = webhookInput(form, stored);
    const onError = (err: unknown) => setErrors(webhookFieldErrors(err));
    const done = (h: Webhook) => onSaved(`Webhook ${h.name} saved.`);
    if (stored) update.mutate({ id: stored.id, body }, { onSuccess: done, onError });
    else create.mutate(body, { onSuccess: done, onError });
  };

  const toggleEvent = (ev: (typeof WEBHOOK_EVENTS)[number]['value'], on: boolean) =>
    set('events', on ? WEBHOOK_EVENTS.map((x) => x.value).filter((x) => x === ev || form.events.includes(x)) : form.events.filter((x) => x !== ev));

  return (
    <Dialog
      title={stored ? (locked ? `Webhook ${stored.name}` : `Edit ${stored.name}`) : 'New webhook'}
      description={stored ? undefined : 'Hangar sends a POST to this address for every event you choose.'}
      onClose={onClose}
      dismissible={!save.isPending}
      wide
    >
      <form className="dialog-form" onSubmit={submit} noValidate>
        <fieldset className="settings-fieldset webhook-form" disabled={locked}>
          <legend className="visually-hidden">Webhook settings</legend>
          <Field id={`${id}-name`} label="Name" error={errors.name}>
            {(a) => <input {...a} className="input" autoComplete="off" value={form.name} onChange={(e) => set('name', e.target.value)} data-autofocus />}
          </Field>

          {stored && !form.replaceUrl ? (
            <div className="field">
              <span className="field__label">URL</span>
              <span className="webhook-stored">
                <code>{stored.url}</code>
                {!locked && (
                  <button type="button" className="button button--secondary button--sm" onClick={() => setForm((f) => ({ ...f, replaceUrl: true, url: '' }))}>
                    Replace URL
                  </button>
                )}
              </span>
              <span className="field__hint">Shown shortened: the full URL can hold a credential, so it is never shown again.</span>
              {errors.url && <span className="field__error">{errors.url}</span>}
            </div>
          ) : (
            <Field
              id={`${id}-url`}
              label={stored ? 'New URL' : 'URL'}
              error={errors.url}
              hint={
                stored ? (
                  <>
                    Replaces {stored.url}.{' '}
                    <button type="button" className="link-button" onClick={() => setForm((f) => ({ ...f, replaceUrl: false, url: '' }))}>
                      Keep the stored URL
                    </button>
                  </>
                ) : (
                  'An http:// or https:// address the server can reach. Internal addresses need http.allow_private_networks'
                )
              }
            >
              {(a) => (
                <input
                  {...a}
                  className="input input--mono"
                  type="url"
                  autoComplete="off"
                  spellCheck={false}
                  placeholder="https://hooks.slack.com/services/…"
                  value={form.url}
                  onChange={(e) => set('url', e.target.value)}
                />
              )}
            </Field>
          )}

          <Field id={`${id}-format`} label="Message format" error={errors.format}>
            {(a) => (
              <Select
                id={a.id}
                value={form.format}
                options={WEBHOOK_FORMATS.map((f) => ({ value: f.value, label: f.label, hint: f.hint }))}
                onChange={(v) => set('format', v)}
                disabled={locked}
                aria-invalid={a['aria-invalid']}
                aria-describedby={a['aria-describedby']}
              />
            )}
          </Field>

          <fieldset className="fieldset" aria-describedby={errors.events ? `${id}-events-msg` : undefined}>
            <legend className="field__label">Events</legend>
            <div className="option-grid">
              {WEBHOOK_EVENTS.map((ev) => {
                const on = form.events.includes(ev.value);
                return (
                  <label key={ev.value} className={on ? 'option is-on' : 'option'}>
                    <input type="checkbox" checked={on} onChange={(e) => toggleEvent(ev.value, e.target.checked)} />
                    <span>
                      <span className="option__title">{ev.label}</span>
                      <span className="option__hint">
                        <code>{ev.value}</code>
                        {ev.hint && ` · ${ev.hint}`}
                      </span>
                    </span>
                  </label>
                );
              })}
            </div>
            {errors.events && (
              <span id={`${id}-events-msg`} className="field__error">
                {errors.events}
              </span>
            )}
          </fieldset>

          <Field id={`${id}-repos`} label="Repositories" optional error={errors.repositories} hint="Names or globs such as npm-*. Empty: every repository you can read">
            {(a) => (
              <ListInput
                id={a.id}
                describedBy={a['aria-describedby']}
                invalid={!!a['aria-invalid']}
                itemLabel="repository"
                values={form.repositories}
                onChange={(v) => set('repositories', v)}
                disabled={locked}
                placeholder="npm-*, maven-releases"
              />
            )}
          </Field>

          {stored?.secret_configured ? (
            <fieldset className="fieldset" aria-describedby={`${id}-secret-msg`}>
              <legend className="field__label">Signing secret</legend>
              <div className="option-grid">
                {SECRET_CHOICES.map((c) => (
                  <label key={c.value} className={form.secretChoice === c.value ? 'option is-on' : 'option'}>
                    <input type="radio" name={`${id}-secret-choice`} checked={form.secretChoice === c.value} onChange={() => set('secretChoice', c.value)} />
                    <span>
                      <span className="option__title">{c.title}</span>
                      <span className="option__hint">{c.hint}</span>
                    </span>
                  </label>
                ))}
              </div>
              {form.secretChoice === 'replace' && (
                <input
                  className="input"
                  type="password"
                  autoComplete="new-password"
                  aria-label="New signing secret"
                  aria-invalid={errors.secret ? true : undefined}
                  aria-describedby={`${id}-secret-msg`}
                  value={form.secret}
                  onChange={(e) => set('secret', e.target.value)}
                />
              )}
              <span id={`${id}-secret-msg`} className={errors.secret ? 'field__error' : renew ? 'field__hint field__hint--warn' : 'field__hint'}>
                {errors.secret ??
                  (renew
                    ? 'The new URL is on another host: the stored secret is only sent where it was entered. Enter it again, or remove it.'
                    : 'Signs each delivery (X-Hangar-Signature: HMAC-SHA256). Never shown again.')}
              </span>
            </fieldset>
          ) : (
            <Field
              id={`${id}-secret`}
              label="Signing secret"
              optional
              error={errors.secret}
              hint="Signs each delivery (X-Hangar-Signature: HMAC-SHA256) so the receiver can check it came from Hangar. Stored encrypted, never shown again"
            >
              {(a) => <input {...a} className="input" type="password" autoComplete="new-password" value={form.secret} onChange={(e) => set('secret', e.target.value)} />}
            </Field>
          )}

          <Toggle checked={form.enabled} onChange={(v) => set('enabled', v)} label="Enabled" disabled={locked} />
          {stored?.disabled_reason && !stored.enabled && (
            <p className="note">
              The server disabled it: {stored.disabled_reason}. Enabling it again resets the failure count.
            </p>
          )}
        </fieldset>

        {save.isError && (
          <ErrorMessage
            error={save.error}
            message={fieldsRefused ? 'Fix the highlighted fields and save again.' : errorText(save.error, { action: 'save the webhook', status: { 409: 'A webhook with this name already exists.' } })}
          />
        )}

        <div className="dialog-actions">
          <button type="button" className="button button--secondary" onClick={onClose} disabled={save.isPending}>
            {locked ? 'Close' : 'Cancel'}
          </button>
          {!locked && (
            <button type="submit" className="button button--primary" disabled={save.isPending} aria-busy={save.isPending || undefined}>
              {save.isPending ? 'Saving…' : stored ? 'Save' : 'Add webhook'}
            </button>
          )}
        </div>
      </form>
    </Dialog>
  );
}
