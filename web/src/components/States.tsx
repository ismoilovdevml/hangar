import type { ReactNode } from 'react';
import { Link, type LinkProps } from '@tanstack/react-router';
import { describeError, rawDetail } from '../lib/apiError';
import { PageHeader } from './PageHeader';

/*
 * The four states a view of data can be in besides showing it, each with its
 * own look so one is never mistaken for another:
 *   - loading: <Loading> (or skeleton rows in a DataTable),
 *   - failed: <ErrorNote> (a load) or <ErrorMessage> (an action),
 *   - empty: <EmptyState> (nothing exists yet),
 *   - filtered to nothing: <NoMatches> (things exist, the filter hides them).
 * A missing entity is <NotFound>.
 */

/** The raw answer behind an error, folded away: what an operator pastes into a bug report. */
export function ErrorDetails({ detail, error }: { detail?: string | null; error?: unknown }) {
  const text = detail ?? rawDetail(error);
  if (!text) return null;
  return (
    <details className="error-details">
      <summary>Details</summary>
      <code>{text}</code>
    </details>
  );
}

/** A failed action (save, test, delete): one plain sentence, and the server's text under Details. */
export function ErrorMessage({ error, message, className = 'form-error' }: { error: unknown; message?: string; className?: string }) {
  if (!error) return null;
  const d = describeError(error);
  return (
    <div className={className} role="alert">
      <span>{message ?? d.message}</span>
      <ErrorDetails detail={d.detail} />
    </div>
  );
}

/** Data on its way. `label` says what: "Loading members…". */
export function Loading({ label = 'Loading…', className }: { label?: string; className?: string }) {
  return (
    <p className={['state-loading', className].filter(Boolean).join(' ')} role="status" aria-live="polite">
      <span className="state-loading__spinner" aria-hidden="true" />
      {label}
    </p>
  );
}

/** Nothing exists yet; says what would appear here and, when the user can, how to add the first one. */
export function EmptyState({ title, children, action }: { title: string; children?: ReactNode; action?: ReactNode }) {
  return (
    <div className="state-empty">
      <p className="state-empty__title">{title}</p>
      {children && <p className="state-empty__text">{children}</p>}
      {action && <div className="state-empty__action">{action}</div>}
    </div>
  );
}

/** Things exist but the current search or filters hide all of them. */
export function NoMatches({ what, onClear, children }: { what: string; onClear?: () => void; children?: ReactNode }) {
  return (
    <div className="state-empty state-empty--filtered" role="status">
      <p className="state-empty__title">No {what} match these filters.</p>
      {children && <p className="state-empty__text">{children}</p>}
      {onClear && (
        <div className="state-empty__action">
          <button type="button" className="button button--secondary button--sm" onClick={onClear}>
            Clear filters
          </button>
        </div>
      )}
    </div>
  );
}

interface NotFoundProps {
  /** "Repository", "Blob store", "Task". */
  kind: string;
  /** The name or id that was asked for. */
  name: string;
  /** Where "Back to …" leads, e.g. `{ to: '/repositories' }`. */
  back: LinkProps;
  /** "repositories" → "Back to repositories". */
  backLabel: string;
  children?: ReactNode;
}

/** An entity that does not exist (or the user cannot see): the same page for every kind. */
export function NotFound({ kind, name, back, backLabel, children }: NotFoundProps) {
  return (
    <div className="screen">
      <PageHeader title={`${kind} not found`} />
      <div className="panel glass state-notfound">
        <p>
          No {kind.toLowerCase()} named <code className="inline-code">{name}</code> exists, or you cannot see it.
        </p>
        {children}
        <p>
          <Link {...back} className="button button--primary">
            Back to {backLabel}
          </Link>
        </p>
      </div>
    </div>
  );
}
