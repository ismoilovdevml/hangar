import { useId, useRef, useState, type FormEvent, type ReactNode } from 'react';
import { ApiRequestError } from '../../api/client';
import { useCanSystem } from '../../api/ops';
import { ConfirmDialog } from '../../components/ConfirmDialog';
import { ErrorNote } from '../../components/ErrorNote';
import { IconPlus } from '../../components/icons';
import { PageHeader } from '../../components/PageHeader';
import { Panel } from '../../components/Panel';
import { Pill } from '../../components/Pill';
import { Field, FileBadge, ListInput, SecretInput, TestResultView } from './controls';
import {
  NEW_PROXY,
  parseProxyAddress,
  proxyAddress,
  proxyForm,
  proxyInput,
  proxyNameFor,
  validateProxy,
  type Errors,
  type ProxyForm,
} from './model';
import { NoPermission } from './NoPermission';
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
  const [notice, setNotice] = useState<string | null>(null);
  const del = useDeleteProxy();

  if (!canRead) return <NoPermission what="outbound proxies" />;

  const list = q.data;
  const items = list?.items ?? [];
  const inUse = del.error instanceof ApiRequestError && del.error.status === 409;
  const startNew = () => {
    setNotice(null);
    setEditing({ mode: 'new' });
  };

  let body: ReactNode;
  if (q.isPending) {
    body = (
      <Panel className="proxy-card" aria-busy="true" aria-label="Outbound proxies">
        <p className="meta">Loading the proxies…</p>
      </Panel>
    );
  } else if (q.isError || !list) {
    body = <ErrorNote what="the proxies" error={q.error} privilege="system:settings:read" onRetry={() => void q.refetch()} />;
  } else {
    body = (
      <>
        {editing && (
          <ProxyEditor
            key={editing.mode === 'edit' ? editing.proxy.name : 'new'}
            editing={editing}
            canEdit={canEdit}
            caFile={list.ca_file}
            taken={items.map((p) => p.name)}
            fileDefault={items.some((p) => p.source === 'file')}
            onClose={() => setEditing(null)}
            onSaved={(msg) => {
              setNotice(msg);
              setEditing(null);
            }}
          />
        )}
        {items.length === 0 ? (
          !editing && <EmptyState environmentProxy={list.environment_proxy} canEdit={canEdit} onAdd={startNew} />
        ) : (
          <ProxyList
            items={items}
            canEdit={canEdit}
            onEdit={(p) => {
              setNotice(null);
              setEditing({ mode: 'edit', proxy: p });
            }}
            onDelete={setDeleting}
          />
        )}
        {items.length > 0 && !list.default && (
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
        {items.length > 0 && canEdit && editing?.mode !== 'new' && (
          <button type="button" className="button button--secondary proxy-add" onClick={startNew}>
            <IconPlus size={18} /> Add another proxy
          </button>
        )}
        {list.ca_file && (
          <p className="meta">
            Every outbound request also trusts <code>{list.ca_file}</code> (http.ca_file).
          </p>
        )}
      </>
    );
  }

  return (
    <div className="screen settings-screen proxy-screen">
      <PageHeader title="Outbound proxy" description="How Hangar reaches nuget.org, npmjs and other remotes." />
      {notice && (
        <p className="form-success" role="status">
          {notice}
        </p>
      )}
      {body}

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
            <p>{deleting.repositories} repositories select this proxy. Point them elsewhere first; deleting is refused while they use it.</p>
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

/* ---------------------------------------------------------------- empty */

function IconCheck() {
  return (
    <svg width="26" height="26" viewBox="0 0 24 24" fill="none" aria-hidden="true">
      <path d="M5 12.5l4.5 4.5L19 7.5" stroke="currentColor" strokeWidth="2.2" strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  );
}

function IconRelay() {
  return (
    <svg width="26" height="26" viewBox="0 0 24 24" fill="none" aria-hidden="true">
      <circle cx="5" cy="12" r="2.5" stroke="currentColor" strokeWidth="2" />
      <circle cx="19" cy="12" r="2.5" stroke="currentColor" strokeWidth="2" />
      <path d="M7.5 12h9M12 8v8" stroke="currentColor" strokeWidth="2" strokeLinecap="round" />
    </svg>
  );
}

/** No proxies: say how remotes are reached today. */
function EmptyState({ environmentProxy, canEdit, onAdd }: { environmentProxy?: string; canEdit: boolean; onAdd: () => void }) {
  const id = useId();
  return (
    <section className="panel glass proxy-empty" aria-labelledby={id}>
      <span className={environmentProxy ? 'proxy-empty__icon proxy-empty__icon--env' : 'proxy-empty__icon'}>{environmentProxy ? <IconRelay /> : <IconCheck />}</span>
      <h2 id={id} className="proxy-empty__title">
        {environmentProxy ? 'Using the server’s proxy' : 'Connecting directly'}
      </h2>
      <p className="proxy-empty__text">
        {environmentProxy ? (
          <>
            Remotes are reached through <code>{environmentProxy}</code> from the server’s HTTPS_PROXY environment. Add a proxy here to manage it in Hangar instead.
          </>
        ) : (
          'Remotes are reachable without a proxy. Add one only if your network blocks direct internet access.'
        )}
      </p>
      {canEdit ? (
        <button type="button" className="button button--primary proxy-empty__add" onClick={onAdd}>
          Add a proxy
        </button>
      ) : (
        <p className="meta">Adding a proxy needs system:settings:admin.</p>
      )}
    </section>
  );
}

/* ---------------------------------------------------------------- list */

function ProxyList({ items, canEdit, onEdit, onDelete }: { items: OutboundProxy[]; canEdit: boolean; onEdit: (p: OutboundProxy) => void; onDelete: (p: OutboundProxy) => void }) {
  const test = useTestProxy();
  const [result, setResult] = useState<{ name: string; result: TestResult } | null>(null);
  const [failed, setFailed] = useState<{ name: string; error: unknown } | null>(null);
  const run = (p: OutboundProxy) => {
    setResult(null);
    setFailed(null);
    test.mutate({ name: p.name }, { onSuccess: (r) => setResult({ name: p.name, result: r }), onError: (error) => setFailed({ name: p.name, error }) });
  };
  return (
    <Panel className="proxy-card proxy-list" aria-label="Outbound proxies">
      <ul className="proxy-list__items" aria-label="Outbound proxies">
        {items.map((p) => {
          const view = p.source === 'file' || !canEdit;
          return (
            <li key={p.name} className="proxy-row" aria-label={p.name}>
              <div className="proxy-row__main">
                <div className="proxy-name">
                  <strong>{p.name}</strong>
                  {p.default && <Pill tone="info">Platform default</Pill>}
                  {p.source === 'file' && <FileBadge />}
                </div>
                <code className="proxy-host">{proxyAddress({ scheme: p.scheme, host: p.host, port: String(p.port) })}</code>
                <span className="meta">
                  {p.repositories === 1 ? '1 repository picks it' : `${p.repositories} repositories pick it`}
                  {p.default && ` · ${p.inheriting} inherit it`}
                </span>
              </div>
              <span className="row-actions">
                <button type="button" className="button button--secondary button--sm" aria-label={`${view ? 'View' : 'Edit'} ${p.name}`} onClick={() => onEdit(p)}>
                  {view ? 'View' : 'Edit'}
                </button>
                {canEdit && (
                  <button type="button" className="button button--secondary button--sm" aria-label={`Test ${p.name}`} onClick={() => run(p)} disabled={test.isPending}>
                    {test.isPending && test.variables?.name === p.name ? 'Testing…' : 'Test'}
                  </button>
                )}
                {canEdit && p.source !== 'file' && (
                  <button type="button" className="button button--ghost-danger button--sm" aria-label={`Delete ${p.name}`} onClick={() => onDelete(p)}>
                    Delete
                  </button>
                )}
              </span>
              {result?.name === p.name && (
                <div className="proxy-row__result" role="region" aria-label={`Test of ${p.name}`}>
                  <TestResultView result={result.result} />
                </div>
              )}
              {failed?.name === p.name && (
                <p className="form-error proxy-row__result" role="alert">
                  {errorMessage(failed.error)}
                </p>
              )}
            </li>
          );
        })}
      </ul>
    </Panel>
  );
}

/* ---------------------------------------------------------------- editor */

const ADDRESS_FIELDS = ['scheme', 'host', 'port'];

function Caret({ open }: { open: boolean }) {
  return (
    <svg className={open ? 'proxy-more__caret is-open' : 'proxy-more__caret'} width="12" height="12" viewBox="0 0 12 12" aria-hidden="true">
      <path d="M4.5 2.5 8 6l-3.5 3.5" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  );
}

function ProxyEditor({
  editing,
  canEdit,
  caFile,
  taken,
  fileDefault,
  onClose,
  onSaved,
}: {
  editing: NonNullable<Editing>;
  canEdit: boolean;
  caFile?: string;
  taken: string[];
  /** The configuration file's proxy exists and is the default. */
  fileDefault: boolean;
  onClose: () => void;
  onSaved: (message: string) => void;
}) {
  const id = useId();
  const creating = editing.mode === 'new';
  const stored = editing.mode === 'edit' ? editing.proxy : null;
  const file = stored?.source === 'file';
  const locked = file || !canEdit;
  const [form, setForm] = useState<ProxyForm>(() => (stored ? proxyForm(stored) : NEW_PROXY));
  const [address, setAddress] = useState(() => (stored ? proxyAddress(proxyForm(stored)) : ''));
  const [nameOpen, setNameOpen] = useState(false);
  const [authOpen, setAuthOpen] = useState(() => !!stored?.username);
  const [moreOpen, setMoreOpen] = useState(() => !!stored && (stored.no_proxy.length > 0 || stored.redirect_hosts.length > 0 || !!stored.ca_pem));
  const [pasteCa, setPasteCa] = useState(() => !!stored?.ca_pem);
  const [urlOpen, setUrlOpen] = useState(false);
  const [testUrl, setTestUrl] = useState(DEFAULT_TEST_URL);
  const [errors, setErrors] = useState<Errors>({});
  const create = useCreateProxy();
  const update = useUpdateProxy();
  const test = useTestProxy();
  const save = creating ? create : update;
  const fileRef = useRef<HTMLInputElement>(null);
  const passwordStored = !!stored?.password_configured;
  const ff = stored?.file_fields ?? [];
  const set = <K extends keyof ProxyForm>(k: K, v: ProxyForm[K]) => setForm((f) => ({ ...f, [k]: v }));

  const parsed = parseProxyAddress(address);
  const host = 'error' in parsed ? '' : parsed.host;
  const name = creating && !nameOpen ? proxyNameFor(host, taken) : form.name;
  const err = (k: string) => errors[k];
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

  /** Credentials typed into the address move to their own fields. */
  const takeCredentials = () => {
    if ('error' in parsed || parsed.username === undefined) return;
    setForm((f) => ({ ...f, username: parsed.username ?? '', password: parsed.password ?? f.password }));
    setAddress(proxyAddress(parsed));
    setAuthOpen(true);
  };

  /** The form with the address applied, and what is wrong with it. */
  const collect = (forTest: boolean): { next: ProxyForm; found: Errors } => {
    const next: ProxyForm = { ...form, name };
    if (!('error' in parsed)) {
      Object.assign(next, { scheme: parsed.scheme, host: parsed.host, port: parsed.port });
      if (parsed.username !== undefined) {
        next.username = parsed.username;
        if (parsed.password !== undefined) next.password = parsed.password;
      }
    }
    const found = validateProxy(next, creating && !forTest, passwordStored);
    const addrErr = 'error' in parsed ? parsed.error : ADDRESS_FIELDS.map((k) => found[k]).find(Boolean);
    for (const k of ADDRESS_FIELDS) delete found[k];
    if (addrErr) found.address = addrErr;
    // Without a host there is no name to suggest yet; the address error says what to do.
    if (creating && !nameOpen && !next.name) delete found.name;
    return { next, found };
  };
  const show = (found: Errors) => {
    const out = { ...found };
    const addrErr = ADDRESS_FIELDS.map((k) => out[k]).find(Boolean);
    for (const k of ADDRESS_FIELDS) delete out[k];
    if (addrErr) out.address = addrErr;
    setErrors(out);
    if (out.name) setNameOpen(true);
    if (out.username || out.password) setAuthOpen(true);
    if (out.no_proxy || out.redirect_hosts || out.ca_pem) setMoreOpen(true);
  };

  const submit = (e: FormEvent) => {
    e.preventDefault();
    const { next, found } = collect(false);
    show(found);
    if (Object.keys(found).length) return;
    setForm(next);
    const body = proxyInput(next);
    const onError = (e2: unknown) => show(fieldErrorsOf(e2));
    if (creating) create.mutate(body, { onSuccess: (p) => onSaved(`Proxy ${p.name} saved and in use on every node.`), onError });
    else update.mutate({ name: next.name, body }, { onSuccess: (p) => onSaved(`Proxy ${p.name} saved and in use on every node.`), onError });
  };

  const runTest = () => {
    const { next, found } = collect(true);
    delete found.name;
    if (!file && Object.keys(found).length) {
      show(found);
      return;
    }
    test.mutate({ name: next.name || 'draft', url: testUrl.trim(), settings: file ? undefined : { ...proxyInput(next), default: false } }, { onError: (e2) => show(fieldErrorsOf(e2)) });
  };

  const testHost = (() => {
    try {
      return new URL(testUrl.trim() || DEFAULT_TEST_URL).host;
    } catch {
      return testUrl;
    }
  })();
  const r = test.data;
  const status = test.isPending
    ? { tone: '', text: `Testing against ${testHost}…` }
    : r
      ? r.ok
        ? { tone: 'ok', text: `${testHost} answered ${r.status_code ?? 'OK'} in ${r.latency_ms} ms` }
        : { tone: 'bad', text: r.message }
      : test.isError && Object.keys(fieldErrorsOf(test.error)).length === 0
        ? { tone: 'bad', text: errorMessage(test.error) }
        : { tone: '', text: `Tests against ${testHost}` };

  const readCa = async (fileList: FileList | null) => {
    const f = fileList?.[0];
    if (!f) return;
    set('ca_pem', (await f.text()).trim());
    setPasteCa(true);
  };

  return (
    <form className="panel glass proxy-card proxy-form" onSubmit={submit} noValidate aria-labelledby={creating ? undefined : `${id}-title`} aria-label={creating ? 'New proxy' : undefined}>
      {stored && (
        <div className="proxy-form__head">
          <h2 id={`${id}-title`} className="proxy-form__title">
            Proxy {stored.name}
          </h2>
          {stored.default && <Pill tone="info">Platform default</Pill>}
        </div>
      )}
      {file && (
        <p className="note settings-file-note">
          Set in the configuration file (<code>http.proxy</code>, <code>http.proxy_username</code>, <code>http.no_proxy</code> in hangar.yaml or HANGAR_HTTP_*). It is
          the platform default and read-only here.
        </p>
      )}
      <fieldset className="settings-fieldset proxy-form__fields" disabled={locked}>
        <legend className="visually-hidden">Proxy settings</legend>
        <div className="field proxy-form__address">
          <label htmlFor={`${id}-addr`} className="proxy-form__label">
            {lbl('Proxy address', 'host')}
          </label>
          <input
            id={`${id}-addr`}
            className="input input--mono"
            autoComplete="off"
            spellCheck={false}
            placeholder="http://proxy.example.com:3128"
            value={address}
            aria-invalid={err('address') ? true : undefined}
            aria-describedby={`${id}-addr-msg`}
            onChange={(e) => setAddress(e.target.value)}
            onBlur={takeCredentials}
          />
          <span id={`${id}-addr-msg`} className={err('address') ? 'field__error' : 'field__hint'}>
            {err('address') ?? 'Same value as HTTPS_PROXY on a server'}
          </span>
        </div>

        {creating && !nameOpen && (
          <p className="meta proxy-form__name">
            {name ? (
              <>
                Saved as <code>{name}</code> ·{' '}
              </>
            ) : (
              'Named after its host · '
            )}
            <button type="button" className="link-button" onClick={() => {
              set('name', name);
              setNameOpen(true);
            }}>
              Choose a name
            </button>
          </p>
        )}
        {creating && nameOpen && (
          <Field id={`${id}-name`} label="Name" error={err('name')} hint="Repositories refer to it by this name; it cannot be changed later">
            {(a) => <input {...a} className="input input--mono" autoComplete="off" spellCheck={false} placeholder="corp-proxy" value={form.name} onChange={(e) => set('name', e.target.value)} />}
          </Field>
        )}

        <div className="proxy-default">
          <span className="proxy-default__text">
            <label htmlFor={`${id}-default`}>{lbl('Use for every proxy repository', 'default')}</label>
            <span id={`${id}-default-hint`} className="proxy-default__hint">
              {fileDefault && !file ? 'The proxy of the configuration file is the default.' : 'Repositories that pick a proxy of their own, or none, keep theirs.'}
            </span>
          </span>
          <input
            id={`${id}-default`}
            type="checkbox"
            checked={form.default}
            disabled={locked || (fileDefault && !file)}
            aria-describedby={`${id}-default-hint`}
            onChange={(e) => set('default', e.target.checked)}
          />
        </div>
        {err('default') && <span className="field__error">{err('default')}</span>}

        <div className="proxy-more">
          <button type="button" className="proxy-more__toggle" aria-expanded={authOpen} aria-controls={`${id}-auth`} onClick={() => setAuthOpen((v) => !v)}>
            <Caret open={authOpen} /> Needs a username and password
          </button>
          {authOpen && (
            <div id={`${id}-auth`} className="form-grid form-grid--proxy">
              <Field id={`${id}-user`} label={lbl('Username', 'username')} error={err('username')}>
                {(a) => <input {...a} className="input" autoComplete="off" value={form.username} onChange={(e) => set('username', e.target.value)} />}
              </Field>
              <Field
                id={`${id}-pw`}
                label={lbl('Password', 'password')}
                error={err('password')}
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
            </div>
          )}

          <button type="button" className="proxy-more__toggle" aria-expanded={moreOpen} aria-controls={`${id}-more`} onClick={() => setMoreOpen((v) => !v)}>
            <Caret open={moreOpen} /> Exceptions and certificates
          </button>
          {moreOpen && (
            <div id={`${id}-more`} className="proxy-more__body">
              <Field id={`${id}-noproxy`} label={lbl('Skip the proxy for', 'no_proxy')} error={err('no_proxy')} hint="Hosts, domain suffixes (.corp.example), IPs and CIDRs reached directly">
                {(a) => (
                  <ListInput id={a.id} describedBy={a['aria-describedby']} invalid={!!a['aria-invalid']} itemLabel="host" values={form.no_proxy} onChange={(v) => set('no_proxy', v)} disabled={locked} placeholder=".corp.example, 10.0.0.0/8" />
                )}
              </Field>
              <Field
                id={`${id}-redirect`}
                label={lbl('Redirect target hosts', 'redirect_hosts')}
                error={err('redirect_hosts')}
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
              {pasteCa ? (
                <Field id={`${id}-ca`} label={lbl('CA certificates', 'ca_pem')} error={err('ca_pem')} hint="PEM; the CA of a TLS-inspecting proxy">
                  {(a) => (
                    <textarea {...a} className="input input--mono textarea" rows={4} spellCheck={false} placeholder="-----BEGIN CERTIFICATE-----" value={form.ca_pem} onChange={(e) => set('ca_pem', e.target.value)} />
                  )}
                </Field>
              ) : null}
              {!locked && (
                <div className="proxy-ca">
                  <input ref={fileRef} type="file" accept=".pem,.crt,.cer,application/x-pem-file" className="visually-hidden" tabIndex={-1} aria-hidden="true" onChange={(e) => void readCa(e.target.files)} />
                  <button type="button" className="proxy-ca__upload" onClick={() => fileRef.current?.click()}>
                    {form.ca_pem ? 'Replace CA certificate (.pem)' : 'Upload CA certificate (.pem)'}
                  </button>
                  {!pasteCa && (
                    <button type="button" className="link-button" onClick={() => setPasteCa(true)}>
                      Paste it instead
                    </button>
                  )}
                </div>
              )}
              {caFile && (
                <p className="meta">
                  Also trusted for every request: <code>{caFile}</code> (http.ca_file).
                </p>
              )}
            </div>
          )}
        </div>
      </fieldset>

      {urlOpen && canEdit && (
        <Field id={`${id}-testurl`} label="Test URL" hint="Fetched through this proxy; the HTTP status and time are shown" error={err('url')}>
          {(a) => <input {...a} className="input input--mono" type="url" spellCheck={false} value={testUrl} onChange={(e) => setTestUrl(e.target.value)} />}
        </Field>
      )}
      {r && !r.ok && <TestResultView result={r} />}
      {!!save.error && (
        <p className="form-error" role="alert">
          {errorMessage(save.error)}
        </p>
      )}

      <div className="proxy-form__footer">
        <p className={`proxy-form__status${status.tone ? ` proxy-form__status--${status.tone}` : ''}`} role="status">
          {status.text}
          {canEdit && !urlOpen && (
            <>
              {' · '}
              <button type="button" className="link-button" onClick={() => setUrlOpen(true)}>
                Change
              </button>
            </>
          )}
        </p>
        <button type="button" className="button button--secondary proxy-form__button" onClick={onClose}>
          {locked ? 'Close' : 'Cancel'}
        </button>
        {canEdit && (
          <button type="button" className="button button--secondary proxy-form__button" onClick={runTest} disabled={test.isPending}>
            {test.isPending ? 'Testing…' : 'Test'}
          </button>
        )}
        {!locked && (
          <button type="submit" className="button button--primary proxy-form__button" disabled={save.isPending}>
            {save.isPending ? 'Saving…' : 'Save'}
          </button>
        )}
      </div>
    </form>
  );
}
