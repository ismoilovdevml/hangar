import type { ReactNode } from 'react';

interface PageHeaderProps {
  title: ReactNode;
  /** The heading's accessible name, when `title` holds more than its words. */
  titleLabel?: string;
  description?: ReactNode;
  eyebrow?: ReactNode;
  actions?: ReactNode;
}

export function PageHeader({ title, titleLabel, description, eyebrow, actions }: PageHeaderProps) {
  return (
    <div className="page-header">
      <div className="page-header__text">
        {eyebrow && <p className="page-header__eyebrow">{eyebrow}</p>}
        <h1 className="page-header__title" aria-label={titleLabel}>
          {title}
        </h1>
        {description && <p className="page-header__desc">{description}</p>}
      </div>
      {actions && <div className="page-header__actions">{actions}</div>}
    </div>
  );
}
