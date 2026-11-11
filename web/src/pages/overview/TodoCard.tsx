import type { ReactNode } from 'react';
import { Link } from '@tanstack/react-router';
import { ErrorNote } from '../../components/ErrorNote';
import { IconCheck, IconClose } from '../../components/icons';
import type { TodoItem, TodoLink } from '../../lib/overview';

/** The link of a to-do item (To do card and the bell). */
export function TodoAnchor({ link, className, children }: { link: TodoLink; className: string; children: ReactNode }) {
  switch (link.kind) {
    case 'account-tokens':
      return (
        <Link to="/account" hash="my-tokens-title" className={className}>
          {children}
        </Link>
      );
    case 'access-tokens':
      return (
        <Link to="/access" search={{ tab: 'tokens' }} className={className}>
          {children}
        </Link>
      );
    case 'audit-pushes':
      return (
        <Link to="/audit" search={{ action: 'component.publish' }} className={className}>
          {children}
        </Link>
      );
    case 'vulnerabilities':
      return (
        <Link to="/vulnerabilities" className={className}>
          {children}
        </Link>
      );
    case 'system-status':
      return (
        <Link to="/system/status" className={className}>
          {children}
        </Link>
      );
    case 'tasks':
      return (
        <Link to="/tasks" className={className}>
          {children}
        </Link>
      );
    case 'task':
      return (
        <Link to="/tasks/$taskId" params={{ taskId: link.id }} className={className}>
          {children}
        </Link>
      );
    case 'blob-stores':
      return (
        <Link to="/blob-stores" className={className}>
          {children}
        </Link>
      );
    case 'repository':
      return (
        <Link to="/repositories/$name/settings" params={{ name: link.name }} className={className}>
          {children}
        </Link>
      );
    case 'docs':
      return (
        <a href={link.href} target="_blank" rel="noreferrer" className={className}>
          {children}
        </a>
      );
  }
}

interface TodoCardProps {
  items?: TodoItem[];
  error: boolean;
  onRetry: () => void;
  /** Hides a dismissible item for good (in this browser). */
  onDismiss: (id: string) => void;
}

/** What needs someone's attention, most urgent first. */
export function TodoCard({ items, error, onRetry, onDismiss }: TodoCardProps) {
  const urgent = items?.some((t) => t.tone === 'bad');
  return (
    <section className="ov-card todo glass" aria-labelledby="todo-title">
      <div className="todo__head">
        <h2 className="todo__title" id="todo-title">
          To do
        </h2>
        {items && items.length > 0 && (
          <span className={`todo__count tone-${urgent ? 'bad' : 'warn'}`}>
            {items.length}
            <span className="visually-hidden"> {items.length === 1 ? 'item' : 'items'}</span>
          </span>
        )}
      </div>
      {!items ? (
        error ? (
          <ErrorNote what="what needs attention" onRetry={onRetry} />
        ) : (
          <div className="todo__loading" aria-busy="true">
            {[0, 1, 2].map((i) => (
              <span key={i} className="skeleton todo__skeleton" />
            ))}
          </div>
        )
      ) : items.length === 0 ? (
        <div className="todo__empty">
          <span className="todo__empty-icon" aria-hidden="true">
            <IconCheck size={18} />
          </span>
          <span className="todo__empty-title">Nothing needs you right now</span>
          <span className="todo__empty-text">Failing pushes, expiring tokens, critical findings and unhealthy upstreams show up here.</span>
        </div>
      ) : (
        <ul className="todo__list">
          {items.map((t) => (
            <li key={t.id} className={t.dismissible ? 'todo__entry todo__entry--dismissible' : 'todo__entry'}>
              <TodoAnchor link={t.link} className={`todo__item todo__item--${t.tone}`}>
                <span className="todo__tag">{t.tag}</span>
                <span className="todo__item-title">{t.title}</span>
                <span className="todo__detail">{t.detail}</span>
              </TodoAnchor>
              {t.dismissible && (
                <button type="button" className="todo__dismiss" aria-label={`Dismiss: ${t.title}`} title="Dismiss" onClick={() => onDismiss(t.id)}>
                  <IconClose size={14} />
                </button>
              )}
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}
