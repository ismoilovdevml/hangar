import { useEffect, useId, useMemo, useState, type FormEvent } from 'react';
import { versionText } from '../../lib/format';
import { useQuery } from '@tanstack/react-query';
import { Link } from '@tanstack/react-router';
import { csrfToken, CSRF_HEADER } from '../../api/client';
import { sessionQuery, useSystemStatus } from '../../api/hooks';
import { CodeBlock } from '../../components/CodeBlock';
import { CopyButton } from '../../components/CopyButton';
import { IconLogo, IconSearch } from '../../components/icons';
import { Segmented } from '../../components/Segmented';
import {
  anchorId,
  curlCommand,
  exampleBody,
  fillPath,
  filterOperations,
  groupByTag,
  listOperations,
  propertyRows,
  queryString,
  schemaNames,
  typeLabel,
  type OpenAPIDoc,
  type Operation,
  type PropertyRow,
} from '../../lib/openapi';
import { Markdown } from '../../components/Markdown';
import { serverBaseUrl } from '../access/tokenSnippets';
import './apidocs.css';

const SPEC_URL = '/api/v1/openapi.json';

/** The OpenAPI document the server publishes (public, no sign-in). */
function useOpenAPI() {
  return useQuery({
    queryKey: ['openapi'],
    queryFn: async ({ signal }) => {
      const res = await fetch(SPEC_URL, { signal, headers: { Accept: 'application/json' }, credentials: 'omit' });
      if (!res.ok) throw new Error(`The server answered ${res.status} for ${SPEC_URL}.`);
      return (await res.json()) as OpenAPIDoc;
    },
    staleTime: 5 * 60_000,
  });
}

type AuthMode = 'session' | 'token';

interface TryAuth {
  mode: AuthMode;
  token: string;
  signedIn: boolean;
}

/**
 * The built-in API reference: every endpoint of the server's OpenAPI
 * document grouped by tag, with parameters, bodies, responses, schemas,
 * copyable curl commands for this server and a "Try it" that calls the API
 * with the current session or a pasted token.
 */
export function ApiDocsPage() {
  const spec = useOpenAPI();
  const session = useQuery(sessionQuery);
  const status = useSystemStatus();
  const base = serverBaseUrl(status.data?.base_url, window.location.origin);
  const signedIn = !!session.data && !session.data.password_change_required;
  const [q, setQ] = useState('');
  const [chosenMode, setMode] = useState<AuthMode | null>(null);
  const [token, setToken] = useState('');
  const searchId = useId();
  const tokenId = useId();

  // Until the user picks, try with the session when there is one.
  const mode: AuthMode = chosenMode ?? (signedIn || session.isPending ? 'session' : 'token');

  const doc = spec.data;
  const ops = useMemo(() => (doc ? listOperations(doc) : []), [doc]);
  const shown = useMemo(() => filterOperations(ops, q), [ops, q]);
  const groups = useMemo(() => (doc ? groupByTag(doc, shown) : []), [doc, shown]);
  const auth: TryAuth = { mode, token, signedIn };
  const Main = signedIn ? 'div' : 'main';

  return (
    // Signed in, the page sits in the app shell (sidebar and breadcrumb); signed out it stands alone.
    <div className={signedIn ? 'api-docs api-docs--in-app' : 'api-docs wall'}>
      {!signedIn && (
        <header className="api-docs__top glass">
          <span className="brand brand--static">
            <span className="brand__mark" aria-hidden="true">
              <IconLogo size={22} />
            </span>
            <span className="brand__text">
              <span className="brand__name">Hangar</span>
              <span className="brand__sub">API reference{doc ? ` · ${versionText(doc.info.version)}` : ''}</span>
            </span>
          </span>
          <nav className="api-docs__links" aria-label="API documents">
            <Link to="/login" search={{ redirect: '/api-docs' }} className="button button--secondary button--sm">
              Sign in
            </Link>
          </nav>
        </header>
      )}

      <div className="api-docs__layout">
        <aside className="api-docs__side panel glass" aria-label="Endpoints">
          <label htmlFor={searchId} className="visually-hidden">
            Search endpoints
          </label>
          <span className="api-docs__search">
            <IconSearch size={16} aria-hidden="true" />
            <input
              id={searchId}
              type="search"
              className="input input--sm"
              placeholder="Search endpoints"
              value={q}
              onChange={(e) => setQ(e.target.value)}
              autoComplete="off"
            />
          </span>
          {doc && (
            <p className="meta" aria-live="polite">
              {q ? `${shown.length} of ${ops.length} endpoints` : `${ops.length} endpoints`}
            </p>
          )}
          <ul className="api-docs__toc">
            <li>
              <a href="#start">Getting started</a>
            </li>
            {groups.map((g) => (
              <li key={g.tag}>
                <a href={`#${anchorId('tag', g.tag)}`}>
                  {g.tag} <span className="meta">{g.operations.length}</span>
                </a>
              </li>
            ))}
            {doc && !q && (
              <li>
                <a href="#schemas">Schemas</a>
              </li>
            )}
          </ul>
        </aside>

        {/* Signed in, the app shell already provides the main landmark. */}
        <Main className="api-docs__main">
          <section id="start" className="panel glass api-docs__intro" aria-labelledby="start-title">
            <h1 id="start-title" className="page-header__title">
              {doc?.info.title ?? 'Hangar API'}
            </h1>
            <p className="page-header__desc">
              Automate this registry: list and create repositories, search packages, manage cleanup and read vulnerability findings with any HTTP
              client.
            </p>
            <dl className="summary__list api-docs__facts">
              <dt>Base URL</dt>
              <dd className="api-docs__copyable">
                <code className="inline-code">{base}/api/v1</code>
                <CopyButton text={`${base}/api/v1`} label="Copy base URL" />
              </dd>
              <dt>Authentication</dt>
              <dd>
                Send an access token: <code className="inline-code">Authorization: Bearer hgr_…</code>. Create one under{' '}
                {signedIn ? (
                  <Link className="link-strong" to="/account" hash="my-tokens-title">
                    My account
                  </Link>
                ) : (
                  'My account'
                )}
                ; it can do what its permissions and its owner’s roles allow, no more. <code className="inline-code">GET /auth/session</code> shows
                what the token may do.
              </dd>
              <dt>Errors</dt>
              <dd>
                Every error has the same body: <code className="inline-code">{'{"code": "not_found", "message": "…"}'}</code>. Retry 429 and 5xx
                answers after <code className="inline-code">Retry-After</code> seconds.
              </dd>
              <dt>Lists</dt>
              <dd>
                Pass <code className="inline-code">limit</code> and the previous page’s <code className="inline-code">next_cursor</code> as{' '}
                <code className="inline-code">cursor</code> until it is null.
              </dd>
              <dt>For automation</dt>
              <dd>
                <a className="link-strong" href="/llms.txt" target="_blank" rel="noreferrer">
                  /llms.txt
                </a>{' '}
                and the{' '}
                <a className="link-strong" href="/api/v1/automation-guide.md" target="_blank" rel="noreferrer">
                  automation guide
                </a>{' '}
                are written for scripts and automated clients, with this server’s URLs. The raw document:{' '}
                <a className="link-strong" href="/api/v1/openapi.yaml" target="_blank" rel="noreferrer">
                  openapi.yaml
                </a>
                ,{' '}
                <a className="link-strong" href="/api/v1/openapi.json" target="_blank" rel="noreferrer">
                  openapi.json
                </a>
                {doc ? ` (version ${doc.info.version})` : ''}.
              </dd>
            </dl>
            <div className="api-docs__auth" role="group" aria-label="Try it with">
              <span className="field__label">Try it with</span>
              <Segmented
                label="Credentials for Try it"
                value={mode}
                onChange={setMode}
                options={[
                  { value: 'session', label: signedIn ? `Your session (${session.data?.user.username})` : 'Your session' },
                  { value: 'token', label: 'An access token' },
                ]}
              />
              {mode === 'token' && (
                <span className="api-docs__token">
                  <label htmlFor={tokenId} className="visually-hidden">
                    Access token
                  </label>
                  <input
                    id={tokenId}
                    type="password"
                    className="input input--sm input--mono"
                    placeholder="hgr_…"
                    value={token}
                    onChange={(e) => setToken(e.target.value.trim())}
                    autoComplete="off"
                    spellCheck={false}
                  />
                </span>
              )}
              {mode === 'session' && !signedIn && !session.isPending && <span className="meta">You are not signed in.</span>}
              {mode === 'token' && <span className="meta">The token stays in this page; it is never stored.</span>}
            </div>
          </section>

          {spec.isPending && (
            <div className="panel glass" aria-busy="true">
              <p className="meta">Loading the API description…</p>
            </div>
          )}
          {spec.isError && (
            <div className="panel glass" role="alert">
              <p className="form-error">Could not load the API description. {spec.error instanceof Error ? spec.error.message : ''}</p>
              <button type="button" className="button button--secondary button--sm" onClick={() => void spec.refetch()}>
                Try again
              </button>
            </div>
          )}
          {doc && groups.length === 0 && (
            <div className="panel glass">
              <p className="meta">No endpoint matches “{q}”.</p>
            </div>
          )}

          {doc &&
            groups.map((g) => (
              <section key={g.tag} id={anchorId('tag', g.tag)} className="api-docs__group" aria-labelledby={`${anchorId('tag', g.tag)}-title`}>
                <h2 id={`${anchorId('tag', g.tag)}-title`} className="api-docs__group-title">
                  {g.tag}
                </h2>
                {g.description && <p className="meta">{g.description}</p>}
                {g.operations.map((op) => (
                  <OperationCard key={op.id} doc={doc} op={op} base={base} auth={auth} defaultOpen={shown.length === 1} />
                ))}
              </section>
            ))}

          {doc && !q && <Schemas doc={doc} />}
        </Main>
      </div>
    </div>
  );
}

/* ---------------------------------------------------------------- operation */

/** Opens a card when the location hash names it (links to an operation or schema). */
function useOpenOnHash(id: string, setOpen: (v: boolean) => void) {
  useEffect(() => {
    const onHash = () => {
      if (window.location.hash === `#${id}`) setOpen(true);
    };
    window.addEventListener('hashchange', onHash);
    return () => window.removeEventListener('hashchange', onHash);
  }, [id, setOpen]);
}

function MethodBadge({ method }: { method: string }) {
  return <span className={`method method--${method.toLowerCase()}`}>{method}</span>;
}

function OperationCard({ doc, op, base, auth, defaultOpen }: { doc: OpenAPIDoc; op: Operation; base: string; auth: TryAuth; defaultOpen: boolean }) {
  const id = anchorId('op', op.id);
  // null: not toggled yet, so open when it is the only search result or linked to.
  const [toggled, setOpen] = useState<boolean | null>(window.location.hash === `#${id}` ? true : null);
  useOpenOnHash(id, setOpen);
  const open = toggled ?? defaultOpen;
  return (
    <details id={id} className="op panel glass" open={open} onToggle={(e) => setOpen(e.currentTarget.open)}>
      <summary className="op__summary">
        <MethodBadge method={op.method} />
        <code className="op__path">{op.path}</code>
        <span className="op__title">{op.summary || op.id}</span>
        {op.public && <span className="pill tone-sage">Public</span>}
      </summary>
      {open && <OperationBody doc={doc} op={op} base={base} auth={auth} />}
    </details>
  );
}

function OperationBody({ doc, op, base, auth }: { doc: OpenAPIDoc; op: Operation; base: string; auth: TryAuth }) {
  const [pathValues, setPathValues] = useState<Record<string, string>>({});
  const [queryValues, setQueryValues] = useState<Record<string, string>>({});
  const [body, setBody] = useState(() => exampleBody(doc, op));
  const curl = curlCommand(op, { base, pathValues, queryValues, body: op.body ? body : undefined });
  const pathParams = op.parameters.filter((p) => p.in === 'path');
  const queryParams = op.parameters.filter((p) => p.in === 'query');
  const bodyRows = op.body?.schema ? propertyRows(doc, op.body.schema) : [];

  return (
    <div className="op__body">
      {op.description && (
        <div className="op__desc">
          <Markdown source={op.description} />
        </div>
      )}
      <p className="meta">
        <code className="inline-code">{op.id}</code> · {op.public ? 'No sign-in needed.' : 'Needs an access token or a session.'}
      </p>

      {op.parameters.length > 0 && (
        <div className="op__section">
          <h3 className="op__heading">Parameters</h3>
          <PropsTable
            rows={op.parameters.map((p) => ({
              name: p.name,
              type: `${typeLabel(doc, p.schema)} · ${p.in}`,
              required: !!p.required || p.in === 'path',
              description: p.description ?? '',
            }))}
          />
        </div>
      )}

      {op.body && (
        <div className="op__section">
          <h3 className="op__heading">
            Request body <span className="meta">{op.body.contentType}</span>
            {op.body.schema?.$ref && (
              <>
                {' '}
                · <SchemaLink name={typeLabel(doc, op.body.schema)} />
              </>
            )}
          </h3>
          {bodyRows.length > 0 && <PropsTable rows={bodyRows} />}
        </div>
      )}

      <div className="op__section">
        <h3 className="op__heading">Responses</h3>
        <ul className="op__responses">
          {op.responses.map((r) => (
            <li key={r.status}>
              <span className={`status-code status-code--${r.status[0]}`}>{r.status}</span> {r.description}
              {r.schema && (
                <span className="meta">
                  {' '}
                  · {r.schema.$ref ? <SchemaLink name={typeLabel(doc, r.schema)} /> : typeLabel(doc, r.schema)}
                </span>
              )}
            </li>
          ))}
        </ul>
      </div>

      <div className="op__section">
        <h3 className="op__heading">Example</h3>
        <CodeBlock label={`curl command for ${op.id}`} lines={curl.split('\n').map((text) => ({ text }))} />
      </div>

      <TryIt
        op={op}
        auth={auth}
        pathParams={pathParams.map((p) => p.name)}
        queryParams={queryParams.map((p) => p.name)}
        pathValues={pathValues}
        setPathValues={setPathValues}
        queryValues={queryValues}
        setQueryValues={setQueryValues}
        body={body}
        setBody={setBody}
      />
    </div>
  );
}

function SchemaLink({ name }: { name: string }) {
  return (
    <a className="link-strong" href={`#${anchorId('schema', name)}`}>
      {name}
    </a>
  );
}

function PropsTable({ rows }: { rows: PropertyRow[] }) {
  return (
    <div className="table-scroll">
      <table className="props">
        <thead>
          <tr>
            <th scope="col">Name</th>
            <th scope="col">Type</th>
            <th scope="col">Description</th>
          </tr>
        </thead>
        <tbody>
          {rows.map((r) => (
            <tr key={r.name}>
              <td>
                <code className="mono">{r.name}</code>
                {r.required && <span className="props__req">required</span>}
              </td>
              <td className="mono">{r.ref ? <SchemaLink name={r.type} /> : r.type}</td>
              <td className="props__desc">{r.description ? <Markdown source={r.description} /> : <span className="meta">—</span>}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

/* ---------------------------------------------------------------- try it */

interface TryResult {
  status: number;
  statusText: string;
  ms: number;
  contentType: string;
  body: string;
}

const MAX_SHOWN = 20_000;

/** Calls the API from the browser with the chosen credentials and returns what came back. */
async function send(op: Operation, url: string, auth: TryAuth, body: string): Promise<TryResult> {
  const headers: Record<string, string> = { Accept: 'application/json' };
  const init: RequestInit = { method: op.method, headers, credentials: auth.mode === 'session' ? 'same-origin' : 'omit' };
  if (auth.mode === 'token' && auth.token) headers.Authorization = `Bearer ${auth.token}`;
  if (auth.mode === 'session' && op.method !== 'GET') {
    const t = csrfToken();
    if (t) headers[CSRF_HEADER] = t;
  }
  if (op.body && body.trim()) {
    headers['Content-Type'] = op.body.contentType;
    init.body = body;
  }
  const started = performance.now();
  const res = await fetch(url, init);
  const text = await res.text();
  const contentType = res.headers.get('Content-Type') ?? '';
  let shown = text;
  if (contentType.includes('json') && text) {
    try {
      shown = JSON.stringify(JSON.parse(text), null, 2);
    } catch {
      shown = text;
    }
  }
  if (shown.length > MAX_SHOWN) shown = `${shown.slice(0, MAX_SHOWN)}\n… (${shown.length - MAX_SHOWN} more characters)`;
  return { status: res.status, statusText: res.statusText, ms: Math.round(performance.now() - started), contentType, body: shown };
}

/** Operations a browser cannot usefully call from this page. */
function untryable(op: Operation): string | null {
  if (op.path === '/events') return 'This endpoint streams server-sent events; try it with curl -N.';
  if (op.path.startsWith('/auth/oidc')) return 'This endpoint redirects the browser to the identity provider.';
  if (op.body && !op.body.contentType.includes('json')) return `This endpoint takes ${op.body.contentType}; use the curl example.`;
  return null;
}

interface TryItProps {
  op: Operation;
  auth: TryAuth;
  pathParams: string[];
  queryParams: string[];
  pathValues: Record<string, string>;
  setPathValues: (v: Record<string, string>) => void;
  queryValues: Record<string, string>;
  setQueryValues: (v: Record<string, string>) => void;
  body: string;
  setBody: (v: string) => void;
}

function TryIt({ op, auth, pathParams, queryParams, pathValues, setPathValues, queryValues, setQueryValues, body, setBody }: TryItProps) {
  const [result, setResult] = useState<TryResult | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const formId = useId();
  const blocked = untryable(op);
  const missingPath = pathParams.filter((p) => !pathValues[p]);
  const noCreds = !op.public && (auth.mode === 'session' ? !auth.signedIn : !auth.token);
  const bodyError = op.body && body.trim() ? jsonError(body) : null;

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (blocked || missingPath.length || bodyError) return;
    setBusy(true);
    setError(null);
    try {
      const url = `/api/v1${fillPath(op.path, pathValues)}${queryString(queryValues)}`;
      setResult(await send(op, url, auth, body));
    } catch (err) {
      setResult(null);
      setError(err instanceof Error ? err.message : 'The request failed.');
    } finally {
      setBusy(false);
    }
  };

  if (blocked) {
    return (
      <div className="op__section try">
        <h3 className="op__heading">Try it</h3>
        <p className="meta">{blocked}</p>
      </div>
    );
  }

  return (
    <form className="op__section try" onSubmit={submit} aria-label={`Try ${op.id}`}>
      <h3 className="op__heading">Try it</h3>
      {(pathParams.length > 0 || queryParams.length > 0) && (
        <div className="try__fields">
          {[...pathParams.map((n) => ['path', n] as const), ...queryParams.map((n) => ['query', n] as const)].map(([where, name]) => {
            const id = `${formId}-${where}-${name}`;
            const values = where === 'path' ? pathValues : queryValues;
            const set = where === 'path' ? setPathValues : setQueryValues;
            return (
              <div key={id} className="field">
                <label htmlFor={id} className="field__label">
                  {name} <span className="meta">{where}</span>
                </label>
                <input id={id} className="input input--sm input--mono" value={values[name] ?? ''} onChange={(e) => set({ ...values, [name]: e.target.value })} />
              </div>
            );
          })}
        </div>
      )}
      {op.body && (
        <div className="field">
          <label htmlFor={`${formId}-body`} className="field__label">
            Body
          </label>
          <textarea
            id={`${formId}-body`}
            className="textarea input--mono try__body"
            rows={Math.min(14, Math.max(4, body.split('\n').length))}
            value={body}
            onChange={(e) => setBody(e.target.value)}
            spellCheck={false}
            aria-invalid={bodyError ? true : undefined}
          />
          {bodyError && <span className="field__error">{bodyError}</span>}
        </div>
      )}
      {op.method !== 'GET' && <p className="meta">This sends a real {op.method} request to this server, as the chosen credentials.</p>}
      <div className="try__actions">
        <button type="submit" className="button button--primary button--sm" disabled={busy || missingPath.length > 0 || !!bodyError}>
          {busy ? 'Sending…' : 'Send request'}
        </button>
        {missingPath.length > 0 && <span className="meta">Fill in {missingPath.join(', ')}.</span>}
        {noCreds && missingPath.length === 0 && (
          <span className="meta">{auth.mode === 'session' ? 'Sign in, or paste a token above.' : 'Paste a token above, or the call answers 401.'}</span>
        )}
      </div>
      {error && <p className="form-error">{error}</p>}
      {result && (
        <div className="try__result" aria-live="polite">
          <p className="try__status">
            <span className={`status-code status-code--${String(result.status)[0]}`}>{result.status}</span> {result.statusText}{' '}
            <span className="meta">
              · {result.ms} ms{result.contentType ? ` · ${result.contentType.split(';')[0]}` : ''}
            </span>
          </p>
          {result.body ? (
            <pre className="try__body-out">
              <code>{result.body}</code>
            </pre>
          ) : (
            <p className="meta">No body.</p>
          )}
        </div>
      )}
    </form>
  );
}

function jsonError(text: string): string | null {
  try {
    JSON.parse(text);
    return null;
  } catch (e) {
    return `Not valid JSON: ${e instanceof Error ? e.message : 'parse error'}`;
  }
}

/* ---------------------------------------------------------------- schemas */

function Schemas({ doc }: { doc: OpenAPIDoc }) {
  const names = schemaNames(doc);
  return (
    <section id="schemas" className="api-docs__group" aria-labelledby="schemas-title">
      <h2 id="schemas-title" className="api-docs__group-title">
        Schemas
      </h2>
      <p className="meta">The objects requests and responses carry. {names.length} in total.</p>
      {names.map((n) => (
        <SchemaCard key={n} doc={doc} name={n} />
      ))}
    </section>
  );
}

function SchemaCard({ doc, name }: { doc: OpenAPIDoc; name: string }) {
  const id = anchorId('schema', name);
  const [open, setOpen] = useState(window.location.hash === `#${id}`);
  useOpenOnHash(id, setOpen);
  const schema = doc.components?.schemas?.[name];
  return (
    <details id={id} className="op panel glass" open={open} onToggle={(e) => setOpen(e.currentTarget.open)}>
      <summary className="op__summary">
        <code className="op__path">{name}</code>
        <span className="op__title meta">{typeLabel(doc, { ...schema, description: undefined, $ref: undefined })}</span>
      </summary>
      {open && (
        <div className="op__body">
          {schema?.description && <Markdown source={schema.description} />}
          {propertyRows(doc, schema).length > 0 ? <PropsTable rows={propertyRows(doc, schema)} /> : null}
        </div>
      )}
    </details>
  );
}
