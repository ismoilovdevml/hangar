import { useState, type FormEvent } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { Link, useNavigate, useRouterState, type ErrorComponentProps } from '@tanstack/react-router';
import { ApiRequestError } from '../api/client';
import { sessionQuery } from '../api/hooks';
import { PageHeader } from '../components/PageHeader';
import { ErrorDetails } from '../components/States';
import { describeError } from '../lib/apiError';
import { notFoundSuggestion } from '../lib/notFound';

function PackageSearch() {
  const navigate = useNavigate();
  const [q, setQ] = useState('');
  const submit = (e: FormEvent) => {
    e.preventDefault();
    const v = q.trim();
    void navigate({ to: '/search', search: v ? { q: v } : {} });
  };
  return (
    <form className="state-notfound__search" role="search" aria-label="Search packages" onSubmit={submit}>
      <input className="input" type="search" value={q} onChange={(e) => setQ(e.target.value)} placeholder="Search packages" aria-label="Search packages" />
      <button type="submit" className="button button--secondary">
        Search
      </button>
    </form>
  );
}

/** An address that matches no page, shown inside the app's shell. */
export function NotFoundPage() {
  const path = useRouterState({ select: (s) => s.location.pathname });
  const hint = notFoundSuggestion(path);
  return (
    <div className="screen">
      <PageHeader title="Page not found" />
      <div className="panel glass state-notfound">
        <p>
          No page lives at <code className="inline-code">{path}</code>.
          {hint && (
            <>
              {' '}
              Looking for{' '}
              <Link to={hint.to} search={hint.search ?? {}} className="link-strong link-underline">
                {hint.label}
              </Link>
              ?
            </>
          )}
        </p>
        <PackageSearch />
        <p className="state-notfound__actions">
          <Link to="/" className="button button--primary">
            Go to the overview
          </Link>
          <Link to="/repositories" className="button button--secondary">
            Repositories
          </Link>
        </p>
      </div>
    </div>
  );
}

/** The 404 for addresses outside the signed-in area (no shell around it). */
export function StandaloneNotFound() {
  return (
    <main id="main" className="standalone-screen">
      <div className="screen">
        <PageHeader title="Page not found" />
        <div className="panel glass state-notfound">
          <p>This address does not match any page.</p>
          <p className="state-notfound__actions">
            <Link to="/" className="button button--primary">
              Go to the overview
            </Link>
          </p>
        </div>
      </div>
    </main>
  );
}

/**
 * A page that threw while loading or rendering (a malformed answer, a bug):
 * the shell and navigation stay, with Retry and the error under Details,
 * instead of the router's developer screen.
 */
export function RouteError({ error, reset }: ErrorComponentProps) {
  const qc = useQueryClient();
  const d = describeError(error);
  // A rendering failure is not something the user did: say so, and keep the cause for the report.
  const message = error instanceof ApiRequestError ? d.message : 'Part of the server’s answer was not what this page expects, so it cannot be shown.';
  const retry = () => {
    // Start over with fresh answers; the session stays so the shell does not flicker.
    const session = JSON.stringify(sessionQuery.queryKey);
    void qc.resetQueries({ predicate: (q) => JSON.stringify(q.queryKey) !== session });
    reset();
  };
  return (
    <div className="screen">
      <PageHeader title="This page could not be shown" />
      <div className="panel glass state-notfound" role="alert">
        <p>{message}</p>
        <ErrorDetails detail={d.detail} />
        <p className="state-notfound__actions">
          <button type="button" className="button button--primary" onClick={retry}>
            Retry
          </button>
          <Link to="/" className="button button--secondary">
            Go to the overview
          </Link>
        </p>
      </div>
    </div>
  );
}
