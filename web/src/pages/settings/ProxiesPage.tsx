import { useId, useState, type FormEvent } from 'react';
import { ApiRequestError } from '../../api/client';
import { useCanSystem } from '../../api/ops';
import { ConfirmDialog } from '../../components/ConfirmDialog';
import { DataTable, type Column } from '../../components/DataTable';
import { ErrorNote } from '../../components/ErrorNote';
import { IconPlus } from '../../components/icons';
import { NotAvailable } from '../../components/NotAvailable';
import { PageHeader } from '../../components/PageHeader';
import { Panel } from '../../components/Panel';
import { Pill } from '../../components/Pill';
import { Field, FileBadge, ListInput, SecretInput, TestResultView, Toggle } from './controls';
import { hostPort, NEW_PROXY, proxyForm, proxyInput, validateProxy, type Errors, type ProxyForm } from './model';
import { NoPermission } from './NoPermission';
import { Select } from '../../components/Select';
import {
  errorMessage,
  fieldErrorsOf,
  useCreateProxy,
  useDeleteProxy,
  useProxies,
  useTestProxy,
  useUpdateProxy,
  type OutboundProxy,
  type TestResult,
} from './settingsApi';
import './settings.css';

export const DEFAULT_TEST_URL = 'https://api.nuget.org/v3/index.json';

type Editing = { mode: 'new' } | { mode: 'edit'; proxy: OutboundProxy } | null;

export function ProxiesPage() {
  const canRead = useCanSystem('settings', 'read');
  const canEdit = useCanSystem('settings', 'admin');
  const q = useProxies(canRead);
  const [editing, setEditing] = useState<Editing>(null);
  const [deleting, setDeleting] = useState<OutboundProxy | null>(null);
  const [rowTest, setRowTest] = useState<{ name: string; result: TestResult } | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const test = useTestProxy();
  const del = useDeleteProxy();

  if (!canRead) return <NoPermission what="outbound proxies" />;

  const list = q.data;
  const runRowTest = (p: OutboundProxy) => {
    setRowTest(null);
    test.mutate({ name: p.name }, { onSuccess: (result) => setRowTest({ name: p.name, result }) });
  };

  const columns: Column<OutboundProxy>[] = [
    {
      key: 'name',
      header: 'Name',
      className: 'col-first',
      cell: (p) => (
        <div className="proxy-name">
          <strong>{p.name}</strong>
          {p.source === 'file' && <FileBadge />}
        </div>
      ),
    },
    { key: 'host', header: 'Host', cell: (p) => <code className="proxy-host">{`${p.scheme}://${p.host}`}</code> },
    { key: 'port', header: 'Port', align: 'right', className: 'nowrap', cell: (p) => p.port },
    {
      key: 'default',
      header: 'Default for',
      className: 'nowrap',
      cell: (p) => (p.default ? <Pill tone="info">Platform default</Pill> : <NotAvailable reason="not a default" />),
    },
    {
      key: 'repos',
      header: 'Repositories',
      className: 'nowrap',
      cell: (p) => (
        <span>
          {p.repositories} by name
          {p.default && <span className="meta"> · {p.inheriting} inherit</span>}
        </span>
      ),
    },
    {
      key: 'actions',
      header: <span className="visually-hidden">Actions</span>,
      align: 'right',
      className: 'col-last nowrap',
      cell: (p) => (
        <span className="row-actions">
          <button
            type="button"
            className="button button--secondary button--sm"
            aria-label={`${p.source === 'file' || !canEdit ? 'View' : 'Edit'} ${p.name}`}
            onClick={() => setEditing({ mode: 'edit', proxy: p })}
          >
            {p.source === 'file' || !canEdit ? 'View' : 'Edit'}
          </button>
          {canEdit && (
            <button type="button" className="button button--secondary button--sm" aria-label={`Test ${p.name}`} onClick={() => runRowTest(p)} disabled={test.isPending}>
              Test
            </button>
          )}
          {canEdit && p.source !== 'file' && (
            <button type="button" className="button button--ghost-danger button--sm" aria-label={`Delete ${p.name}`} onClick={() => setDeleting(p)}>
              Delete
            </button>
          )}
        </span>
      ),
    },
  ];

  const inUse = del.error instanceof ApiRequestError && del.error.status === 409;

  return (
    <div className="screen settings-screen">
      <PageHeader
        eyebrow="Settings"
        title="Proxies"
        description="Outbound proxies proxy repositories use to reach their remotes. Repositories inherit the platform default unless they pick a proxy or a direct connection."
        actions={
          canEdit && (
            <button type="button" className="button button--primary" onClick={() => setEditing({ mode: 'new' })} disabled={editing?.mode === 'new'}>
              <IconPlus size={18} /> New proxy
            </button>
          )
        }
      />

      {notice && (
        <p className="notice" role="status">
          {notice}
        </p>
      )}

      {list && !list.default && (
        <p className="note">
          No platform default.{' '}
          {list.environment_proxy ? (
            <>
              Repositories that inherit use <code>{list.environment_proxy}</code> from the server's HTTPS_PROXY environment.
            </>
          ) : (
            'Repositories that inherit connect directly.'
          )}
        </p>
      )}

      {editing && (
        <ProxyEditor
          key={editing.mode === 'edit' ? editing.proxy.name : 'new'}
          editing={editing}
          canEdit={canEdit}
          caFile={list?.ca_file}
          onClose={() => setEditing(null)}
          onSaved={(msg) => {
            setNotice(msg);
            setEditing(null);
          }}
        />
      )}

      {rowTest && (
        <Panel title={`Test of ${rowTest.name}`} titleId="row-test-title">
          <TestResultView result={rowTest.result} />
        </Panel>
      )}
      {test.isError && !editing && (
        <p className="form-error" role="alert">
          {errorMessage(test.error)}
        </p>
      )}

      <Panel flush title="Proxies" titleId="proxies-title" aria-busy={q.isPending || undefined}>
        {q.isError ? (
          <div className="panel-note">
            <ErrorNote what="the proxies" onRetry={() => void q.refetch()} />
          </div>
        ) : (
          <DataTable
            caption="Outbound proxies"
            columns={columns}
            rows={list?.items ?? []}
            rowKey={(p) => p.name}
            minWidth={760}
            empty={q.isPending ? 'Loading…' : 'No proxies yet. Repositories connect directly unless the server environment sets HTTPS_PROXY.'}
          />
        )}
      </Panel>
      {list?.ca_file && (
        <p className="meta">
          Every outbound request also trusts <code>{list.ca_file}</code> (http.ca_file).
        </p>
      )}

      {deleting && (
        <ConfirmDialog
          title={`Delete proxy ${deleting.name}?`}
          confirmLabel="Delete proxy"
          tone="danger"
          busy={del.isPending}
          error={del.error ? errorMessage(del.error) : null}
          onCancel={() => {
            setDeleting(null);
            del.reset();
          }}
          onConfirm={() =>
            del.mutate(deleting.name, {
              onSuccess: () => {
                setNotice(`Proxy ${deleting.name} deleted.`);
                setDeleting(null);
              },
            })
          }
        >
          {inUse ? (
            <p>Point these repositories to another proxy in their settings first, then delete it.</p>
          ) : deleting.repositories > 0 ? (
            <p>
              {deleting.repositories} repositories select this proxy. Point them elsewhere first; deleting is refused while they use it.
            </p>
          ) : (
            <p>
              {deleting.default
                ? 'This is the platform default. Repositories that inherit will connect directly (or through HTTPS_PROXY) afterwards.'
                : 'No repository selects this proxy.'}
            </p>
          )}
        </ConfirmDialog>
      )}
    </div>
  );
}

function ProxyEditor({
  editing,
  canEdit,
  caFile,
  onClose,
  onSaved,
}: {
  editing: NonNullable<Editing>;
  canEdit: boolean;
  caFile?: string;
  onClose: () => void;
  onSaved: (message: string) => void;
}) {
  const id = useId();
  const creating = editing.mode === 'new';
  const stored = editing.mode === 'edit' ? editing.proxy : null;
  const file = stored?.source === 'file';
  const locked = file || !canEdit;
  const [form, setForm] = useState<ProxyForm>(() => (stored ? proxyForm(stored) : NEW_PROXY));
  const [errors, setErrors] = useState<Errors>({});
  const [testUrl, setTestUrl] = useState(DEFAULT_TEST_URL);
  const create = useCreateProxy();
  const update = useUpdateProxy();
  const test = useTestProxy();
  const save = creating ? create : update;
  const passwordStored = !!stored?.password_configured;
  const set = <K extends keyof ProxyForm>(k: K, v: ProxyForm[K]) => setForm((f) => ({ ...f, [k]: v }));
  const err = (k: string) => errors[k];
  const ff = stored?.file_fields ?? [];
  const lbl = (text: string, field: string) => (
    <>
      {text}
      {ff.includes(field) && (
        <>
          {' '}
          <FileBadge />
        </>
      )}
    </>
  );

  const submit = (e: FormEvent) => {
    e.preventDefault();
    const found = validateProxy(form, creating, passwordStored);
    setErrors(found);
    if (Object.keys(found).length) return;
    const body = proxyInput(form);
    const onError = (e2: unknown) => setErrors(fieldErrorsOf(e2));
    if (creating) create.mutate(body, { onSuccess: (p) => onSaved(`Proxy ${p.name} created and in use on every node.`), onError });
    else update.mutate({ name: form.name, body }, { onSuccess: (p) => onSaved(`Proxy ${p.name} saved and in use on every node.`), onError });
  };

  const runTest = () => {
    const found = validateProxy(form, creating, passwordStored);
    if (!file && Object.keys(found).length) {
      setErrors(found);
      return;
    }
    const name = form.name.trim() || 'draft';
    test.mutate({ name, url: testUrl.trim(), settings: file ? undefined : { ...proxyInput(form), default: false } }, {
      onError: (e2) => setErrors(fieldErrorsOf(e2)),
    });
  };

  return (
    <Panel title={creating ? 'New proxy' : `Proxy ${stored?.name}`} titleId="proxy-editor-title">
      <form className="settings-form" onSubmit={submit} noValidate aria-labelledby="proxy-editor-title">
        {file && (
          <p className="note settings-file-note">
            Set in the configuration file (<code>http.proxy</code>, <code>http.proxy_username</code>, <code>http.no_proxy</code> in hangar.yaml or HANGAR_HTTP_*). It is
            the platform default and read-only here.
          </p>
        )}
        <fieldset className="settings-fieldset" disabled={locked}>
          <legend className="visually-hidden">Proxy settings</legend>
          <div className="form-grid form-grid--proxy">
            <Field id={`${id}-name`} label="Name" error={err('name')} hint={creating ? 'Cannot be changed later' : undefined}>
              {(a) => (
                <input
                  {...a}
                  className="input input--mono"
                  autoComplete="off"
                  spellCheck={false}
                  value={form.name}
                  readOnly={!creating}
                  placeholder="corp-proxy"
                  onChange={(e) => set('name', e.target.value)}
                />
              )}
            </Field>
            <Field id={`${id}-scheme`} label={lbl('Scheme', 'scheme')} error={err('scheme')} hint="How Hangar talks to the proxy">
              {(a) => (
                <Select
                  id={a.id}
                  aria-describedby={a['aria-describedby']}
                  value={form.scheme}
                  onChange={(v) => set('scheme', v)}
                  disabled={locked}
                  options={[
                    { value: 'http', label: 'http' },
                    { value: 'https', label: 'https' },
                  ]}
                />
              )}
            </Field>
            <Field id={`${id}-host`} label={lbl('Host', 'host')} error={err('host')}>
              {(a) => (
                <input {...a} className="input input--mono" autoComplete="off" spellCheck={false} placeholder="proxy.example.com" value={form.host} onChange={(e) => set('host', e.target.value)} />
              )}
            </Field>
            <Field id={`${id}-port`} label={lbl('Port', 'port')} error={err('port')}>
              {(a) => <input {...a} className="input input--mono" inputMode="numeric" value={form.port} onChange={(e) => set('port', e.target.value)} />}
            </Field>
            <Field id={`${id}-user`} label={lbl('Username', 'username')} error={err('username')} optional>
              {(a) => <input {...a} className="input" autoComplete="off" value={form.username} onChange={(e) => set('username', e.target.value)} />}
            </Field>
            <Field
              id={`${id}-pw`}
              label={lbl('Password', 'password')}
              error={err('password')}
              optional
              hint={passwordStored ? 'A password is stored. Type a new one to replace it.' : 'Stored encrypted; never shown again'}
            >
              {(a) => (
                <SecretInput
                  id={a.id}
                  a11y={a}
                  configured={passwordStored}
                  value={form.password}
                  onChange={(v) => set('password', v)}
                  cleared={form.clear_password}
                  onClear={(v) => set('clear_password', v)}
                  disabled={locked}
                />
              )}
            </Field>
            <Field id={`${id}-noproxy`} label={lbl('No proxy for', 'no_proxy')} error={err('no_proxy')} optional hint="Hosts, domain suffixes (.corp.example), IPs and CIDRs reached directly" wide>
              {(a) => (
                <ListInput id={a.id} describedBy={a['aria-describedby']} invalid={!!a['aria-invalid']} itemLabel="host" values={form.no_proxy} onChange={(v) => set('no_proxy', v)} disabled={locked} placeholder=".corp.example" />
              )}
            </Field>
            <Field
              id={`${id}-redirect`}
              label={lbl('Redirect target hosts', 'redirect_hosts')}
              error={err('redirect_hosts')}
              optional
              wide
              hint="Hosts upstream redirects may lead to and still use this proxy, e.g. a registry's CDN. When set, other hosts are reached directly; empty: every host uses the proxy."
            >
              {(a) => (
                <ListInput
                  id={a.id}
                  describedBy={a['aria-describedby']}
                  invalid={!!a['aria-invalid']}
                  itemLabel="redirect host"
                  values={form.redirect_hosts}
                  onChange={(v) => set('redirect_hosts', v)}
                  disabled={locked}
                  placeholder=".cloudfront.net"
                />
              )}
            </Field>
            <Field id={`${id}-ca`} label={lbl('CA certificates', 'ca_pem')} error={err('ca_pem')} optional wide hint="PEM; the CA of a TLS-inspecting proxy">
              {(a) => (
                <textarea {...a} className="input input--mono textarea" rows={4} spellCheck={false} placeholder="-----BEGIN CERTIFICATE-----" value={form.ca_pem} onChange={(e) => set('ca_pem', e.target.value)} />
              )}
            </Field>
          </div>
          {caFile && (
            <p className="meta">
              Also trusted for every request: <code>{caFile}</code> (http.ca_file).
            </p>
          )}
          <Toggle checked={form.default} onChange={(v) => set('default', v)} label={lbl('Platform default for repositories that inherit', 'default')} disabled={locked} />
          {err('default') && <span className="field__error">{err('default')}</span>}
        </fieldset>

        {canEdit && (
          <div className="proxy-test">
            <Field id={`${id}-testurl`} label="Test URL" hint="Fetched through this proxy; the HTTP status and time are shown" error={err('url')}>
              {(a) => <input {...a} className="input input--mono" type="url" spellCheck={false} value={testUrl} onChange={(e) => setTestUrl(e.target.value)} />}
            </Field>
            <button type="button" className="button button--secondary" onClick={runTest} disabled={test.isPending}>
              {test.isPending ? 'Testing…' : 'Test'}
            </button>
          </div>
        )}
        {test.data && <TestResultView result={test.data} />}
        {test.isError && Object.keys(fieldErrorsOf(test.error)).length === 0 && (
          <p className="form-error" role="alert">
            {errorMessage(test.error)}
          </p>
        )}
        {!!save.error && (
          <p className="form-error" role="alert">
            {errorMessage(save.error)}
          </p>
        )}

        <div className="settings-actions">
          <button type="button" className="button button--secondary" onClick={onClose}>
            {locked ? 'Close' : 'Cancel'}
          </button>
          {!locked && (
            <button type="submit" className="button button--primary" disabled={save.isPending}>
              {save.isPending ? 'Saving…' : creating ? 'Create proxy' : 'Save'}
            </button>
          )}
        </div>
        {stored && (
          <p className="meta">
            {hostPort(stored.host, stored.port)} · used by {stored.repositories} repositories by name
            {stored.updated_at && stored.source !== 'file' && ` · last saved by ${stored.updated_by || 'unknown'}`}
          </p>
        )}
      </form>
    </Panel>
  );
}
