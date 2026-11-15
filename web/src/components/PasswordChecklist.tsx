import type { PasswordCheck } from '../lib/passwordPolicy';
import { IconCheck } from './icons';
import './passwordChecklist.css';

/** The password rules, ticked off live as the user types; link it to the field with aria-describedby={id}. */
export function PasswordChecklist({ id, checks }: { id: string; checks: PasswordCheck[] }) {
  return (
    <ul id={id} className="policy-checks" aria-label="Password requirements">
      {checks.map((c) => (
        <li key={c.id} className={c.met ? 'policy-checks__item is-met' : 'policy-checks__item'}>
          <span className="policy-checks__mark" aria-hidden="true">
            {c.met ? <IconCheck size={12} /> : null}
          </span>
          {c.label}
          <span className="visually-hidden">{c.met ? ' (met)' : ' (not met yet)'}</span>
        </li>
      ))}
    </ul>
  );
}
