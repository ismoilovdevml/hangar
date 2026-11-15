import { describeError } from '../lib/apiError';
import { isForbidden } from '../lib/gates';
import { NoAccessNote } from './NoAccess';
import { ErrorDetails } from './States';

interface ErrorNoteProps {
  what: string;
  onRetry: () => void;
  /** The failure; a 403 shows who to ask instead of a Retry that cannot help. */
  error?: unknown;
  /** The privilege that would have allowed it, for the 403 note. */
  privilege?: string;
}

/** A load that failed: what could not be loaded, why in plain words, Retry, and the raw answer under Details. */
export function ErrorNote({ what, onRetry, error, privilege }: ErrorNoteProps) {
  if (isForbidden(error)) return <NoAccessNote what={what} privilege={privilege} />;
  const why = error === undefined ? null : describeError(error);
  return (
    <div className="error-note" role="alert">
      <span className="error-note__text">
        Could not load {what}.{why && ` ${why.message}`}
      </span>
      <button type="button" className="button button--secondary button--sm" onClick={onRetry}>
        Retry
      </button>
      {why?.detail && <ErrorDetails detail={why.detail} />}
    </div>
  );
}
