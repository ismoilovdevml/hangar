import { isForbidden } from '../lib/gates';
import { NoAccessNote } from './NoAccess';

interface ErrorNoteProps {
  what: string;
  onRetry: () => void;
  /** The failure; a 403 shows who to ask instead of a Retry that cannot help. */
  error?: unknown;
  /** The privilege that would have allowed it, for the 403 note. */
  privilege?: string;
}

export function ErrorNote({ what, onRetry, error, privilege }: ErrorNoteProps) {
  if (isForbidden(error)) return <NoAccessNote what={what} privilege={privilege} />;
  return (
    <div className="error-note" role="alert">
      <span>Could not load {what}.</span>
      <button type="button" className="button button--secondary button--sm" onClick={onRetry}>
        Retry
      </button>
    </div>
  );
}
