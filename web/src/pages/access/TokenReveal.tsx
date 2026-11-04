import { useState } from 'react';
import { useSystemStatus } from '../../api/hooks';
import { CodeBlock } from '../../components/CodeBlock';
import { CopyButton } from '../../components/CopyButton';
import { Segmented } from '../../components/Segmented';
import { FORMAT_META } from '../../lib/format';
import { useRepositoryTargets, type TokenCreated } from './api';
import { formatDate } from './model';
import { summarizeScopes } from './tokenScopes';
import { automationSnippet, clientSnippets, curlSnippet, envSnippet, serverBaseUrl } from './tokenSnippets';

type RevealTab = 'api' | 'clients' | 'automation';

const TABS: { value: RevealTab; label: string }[] = [
  { value: 'api', label: 'REST API' },
  { value: 'clients', label: 'Package clients' },
  { value: 'automation', label: 'Automation' },
];

/** The base URL clients should use, from the server's status (server.base_url). */
function useServerBaseUrl(): string {
  const status = useSystemStatus();
  return serverBaseUrl(status.data?.base_url, typeof window !== 'undefined' ? window.location.origin : 'https://hangar.example.com');
}

/**
 * The one-time view of a new secret: copy it, then paste-ready snippets with
 * the real server URL for curl, package clients and automated clients.
 */
export function SecretPanel({ token, note, onDone }: { token: TokenCreated; note?: string; onDone: () => void }) {
  const [stored, setStored] = useState(false);
  const [tab, setTab] = useState<RevealTab>('api');
  const base = useServerBaseUrl();
  const repos = useRepositoryTargets(tab === 'clients');
  const { can, cannot } = summarizeScopes(token.scopes, token.read_only);
  const snippets = repos.data ? clientSnippets(repos.data, token.scopes, token.read_only, token.owner, token.secret) : [];

  return (
    <div className="dialog-form">
      {note && <p className="notice">{note}</p>}
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
        <dt>Can</dt>
        <dd>
          <ul className="plain-list">
            {can.map((s) => (
              <li key={s}>{s}</li>
            ))}
            {cannot.map((s) => (
              <li key={s} className="meta">
                {s}
              </li>
            ))}
          </ul>
        </dd>
        {token.allowed_cidrs.length > 0 && (
          <>
            <dt>Works from</dt>
            <dd className="mono">{token.allowed_cidrs.join(', ')}</dd>
          </>
        )}
      </dl>

      <Segmented label="How to use the token" options={TABS} value={tab} onChange={setTab} className="reveal-tabs" />
      {tab === 'api' && (
        <div className="reveal-section">
          <p className="meta">Send it as a Bearer token. Every call is checked against the token’s permissions and audited under {token.owner}.</p>
          <CodeBlock label="curl commands" lines={curlSnippet(base, token.secret)} />
          <p className="field__label">Environment</p>
          <CodeBlock label="environment variables" lines={envSnippet(base, token.secret)} />
        </div>
      )}
      {tab === 'clients' && (
        <div className="reveal-section">
          <p className="meta">Package clients take the token as the password, with any user name.</p>
          {repos.isPending && <p className="meta">Loading repositories…</p>}
          {repos.isError && <p className="form-error">Could not load the repositories for the examples.</p>}
          {repos.data && snippets.length === 0 && (
            <p className="meta">This token cannot read any npm, NuGet, PyPI, Maven or Docker repository, so there is nothing to configure.</p>
          )}
          {snippets.map((s) => (
            <div key={s.format} className="reveal-client">
              <p className="field__label">
                {FORMAT_META[s.format].label} · <span className="mono">{s.repository}</span>
                {s.publishes ? '' : ' (download only)'}
              </p>
              <CodeBlock label={`${FORMAT_META[s.format].label} configuration`} lines={s.lines} />
            </div>
          ))}
        </div>
      )}
      {tab === 'automation' && (
        <div className="reveal-section">
          <p className="field__label">Give this to an automation or agent</p>
          <p className="meta">
            Scripts, CI jobs and other automated clients need only these lines. The guide at{' '}
            <a className="link-strong" href="/llms.txt" target="_blank" rel="noreferrer">
              /llms.txt
            </a>{' '}
            explains authentication, errors and paging; the{' '}
            <a className="link-strong" href="/api/v1/openapi.yaml" target="_blank" rel="noreferrer">
              OpenAPI document
            </a>{' '}
            lists every endpoint.
          </p>
          <CodeBlock label="automation access" lines={automationSnippet(base, token.secret)} />
        </div>
      )}

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
