import { Link } from '@tanstack/react-router';
import { useAllRepositories } from '../../api/catalog';
import { useGate } from '../../api/permissions';
import { DOCS } from '../../lib/docs';
import { GATES } from '../../lib/gates';

/**
 * First run: with no repository there is nothing for the Overview to report,
 * so it says what to do first, above the To do reminders. Hidden while the
 * list loads or cannot be read (the cards below report that), and once there
 * is a repository.
 */
export function OnboardingCard() {
  const repos = useAllRepositories();
  const canCreate = useGate(GATES.createRepository);
  if (!repos.data || repos.data.length > 0) return null;
  return (
    <section className="ov-card onboard glass" aria-labelledby="onboard-title">
      <h2 className="onboard__title" id="onboard-title">
        {canCreate ? 'Create your first repository' : 'No repositories yet'}
      </h2>
      <p className="onboard__text">
        {canCreate
          ? 'Hangar is running and holds no packages yet. Start with a hosted repository for your own packages, or a proxy that caches a public registry such as npmjs.org or Docker Hub; then point a client at it.'
          : 'There is no repository you can read yet. Ask an administrator to create one or to give you access; the client guides show how to connect once there is.'}
      </p>
      <div className="onboard__actions">
        {canCreate && (
          <>
            <Link to="/repositories/new" search={{ type: 'hosted' }} className="button button--primary">
              Create a hosted repository
            </Link>
            <Link to="/repositories/new" search={{ type: 'proxy' }} className="button button--secondary">
              Create a proxy repository
            </Link>
          </>
        )}
        <a href={DOCS.guides} target="_blank" rel="noreferrer" className="button button--ghost">
          Client setup guides
          <span className="visually-hidden"> (opens in a new tab)</span>
        </a>
      </div>
    </section>
  );
}
