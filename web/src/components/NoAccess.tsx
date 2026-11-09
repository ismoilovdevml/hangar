import { Link } from '@tanstack/react-router';
import type { Gate } from '../lib/gates';
import { IconLock } from '../pages/settings/icons';
import { PageHeader } from './PageHeader';

interface NoAccessProps {
  /** What the user cannot open: "scheduled tasks", "the audit log". */
  what: string;
  /** The privilege to ask an administrator for; omitted when it is not known. */
  privilege?: string;
}

/** In place of content the server refused: who to ask, and for what. Nothing to retry. */
export function NoAccessNote({ what, privilege }: NoAccessProps) {
  return (
    <div className="no-access" role="note">
      <IconLock size={18} className="no-access__icon" />
      <p className="no-access__text">
        <span className="no-access__title">You don’t have access to {what}.</span>{' '}
        {privilege ? (
          <>
            Ask an administrator for <code className="inline-code">{privilege}</code>.
          </>
        ) : (
          'Ask an administrator for access.'
        )}
      </p>
    </div>
  );
}

/** A whole page the user's roles do not open. */
export function NoAccessPage({ title, gate }: { title: string; gate: Gate }) {
  return (
    <div className="screen">
      <PageHeader title={title} />
      <div className="panel glass">
        <NoAccessNote what={gate.what} privilege={gate.privilege} />
        <p className="meta no-access__back">
          {gate.instead ? (
            <>
              {gate.instead.text} <Link to={gate.instead.to}>{gate.instead.label}</Link>.
            </>
          ) : (
            <Link to="/">Back to the overview</Link>
          )}
        </p>
      </div>
    </div>
  );
}
