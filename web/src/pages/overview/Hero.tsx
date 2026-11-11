import { useId, useState, type FormEvent, type ReactNode } from 'react';
import { Link, useNavigate } from '@tanstack/react-router';
import { useClientEndpoints } from '../../api/hooks';
import type { Format } from '../../api/types';
import { ClientSetup } from '../../components/ClientSetup';
import { ErrorNote } from '../../components/ErrorNote';
import { IconKey, IconPackage, IconPlus, IconSearch, IconTerminal } from '../../components/icons';
import { isMac } from '../../components/commandPaletteContext';
import { useGate } from '../../api/permissions';
import { GATES } from '../../lib/gates';
import { PHONE_QUERY, useMediaQuery } from '../../lib/useMediaQuery';
import { Dialog } from '../access/ui';

/** The client set-up snippets for the repositories the user can read, in a dialog. */
function ConnectClientDialog({ onClose }: { onClose: () => void }) {
  const q = useClientEndpoints();
  const canCreateRepo = useGate(GATES.createRepository);
  return (
    <Dialog title="Connect a client" description="Point your package manager at this registry." onClose={onClose} wide>
      {q.isError ? (
        <ErrorNote what="client endpoints" error={q.error} onRetry={() => void q.refetch()} />
      ) : q.isPending ? (
        <p className="meta">Loading endpoints…</p>
      ) : q.data.items.length === 0 ? (
        <p className="meta">
          No repositories you can read yet.{' '}
          {canCreateRepo ? (
            <>
              <Link to="/repositories/new">Create one</Link> to get client settings.
            </>
          ) : (
            'Ask an administrator for read access to one.'
          )}
        </p>
      ) : (
        <ClientSetup
          targets={q.data.items.map((e) =>
            e.format === 'docker'
              ? { format: e.format, repository: e.repository, host: e.endpoint }
              : { format: e.format, repository: e.repository, url: e.endpoint },
          )}
        />
      )}
    </Dialog>
  );
}

interface ActionProps {
  tone: 'info' | 'peach' | 'sage' | 'violet';
  icon: ReactNode;
  title: string;
  sub: string;
}

function ActionBody({ tone, icon, title, sub }: ActionProps) {
  return (
    <>
      <span className={`quick-action__tile tone-${tone}`} aria-hidden="true">
        {icon}
      </span>
      <span className="quick-action__text">
        <span className="quick-action__title">{title}</span>
        <span className="quick-action__sub">{sub}</span>
      </span>
    </>
  );
}

/** Search box, qualifier examples and the four things people come here to do. */
export function Hero({ topFormat }: { topFormat?: Format }) {
  const navigate = useNavigate();
  const inputId = useId();
  const [q, setQ] = useState('');
  const [connecting, setConnecting] = useState(false);
  const phone = useMediaQuery(PHONE_QUERY);
  const canCreateRepo = useGate(GATES.createRepository);

  const submit = (e: FormEvent) => {
    e.preventDefault();
    const query = q.trim();
    void navigate({ to: '/search', search: query ? { q: query } : {} });
  };

  const examples = [topFormat && `format:${topFormat}`, 'security:findings', 'unused:>90d'].filter((x): x is string => !!x);

  return (
    <section className="hero glass" aria-labelledby="hero-title">
      <h1 className="hero__title" id="hero-title">
        What do you need today?
      </h1>
      <form className="hero__search" role="search" onSubmit={submit}>
        <label htmlFor={inputId} className="visually-hidden">
          Search packages
        </label>
        <IconSearch size={20} className="hero__search-icon" />
        <input
          id={inputId}
          type="search"
          className="hero__input"
          placeholder={phone ? 'Search packages, CVEs…' : 'Package, version, sha256 digest, CVE or license'}
          value={q}
          onChange={(e) => setQ(e.target.value)}
          autoComplete="off"
          spellCheck={false}
        />
        <kbd className="kbd hero__kbd" title="Opens the command palette anywhere">
          {isMac() ? '⌘K' : 'Ctrl K'}
        </kbd>
      </form>
      <div className="hero__try">
        <span className="hero__try-label">Try:</span>
        {examples.map((x) => (
          <Link key={x} to="/search" search={{ q: x }} className="hero__chip">
            {x}
          </Link>
        ))}
      </div>
      <ul className="quick-actions" aria-label="Quick actions">
        <li>
          <button type="button" className="quick-action" onClick={() => setConnecting(true)}>
            <ActionBody tone="info" icon={<IconTerminal size={18} />} title="Connect a client" sub="NuGet, npm, Docker…" />
          </button>
        </li>
        <li>
          <Link to="/account" hash="my-tokens-title" className="quick-action">
            <ActionBody tone="peach" icon={<IconKey size={18} />} title="Create a token" sub="For CLI and CI" />
          </Link>
        </li>
        {canCreateRepo && (
          <li>
            <Link to="/repositories/new" className="quick-action">
              <ActionBody tone="sage" icon={<IconPlus size={18} />} title="New repository" sub="Hosted, proxy, group" />
            </Link>
          </li>
        )}
        <li>
          <Link to="/browse" className="quick-action">
            <ActionBody tone="violet" icon={<IconPackage size={18} />} title="Browse packages" sub="Every repository and version" />
          </Link>
        </li>
      </ul>
      {connecting && <ConnectClientDialog onClose={() => setConnecting(false)} />}
    </section>
  );
}
