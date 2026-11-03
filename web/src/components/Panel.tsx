import type { ElementType, HTMLAttributes, ReactNode } from 'react';

interface PanelProps extends Omit<HTMLAttributes<HTMLElement>, 'title'> {
  as?: ElementType;
  title?: ReactNode;
  /** Id for the heading; the panel is labelled by it. */
  titleId?: string;
  action?: ReactNode;
  /** Remove inner padding (tables that run edge to edge). */
  flush?: boolean;
}

export function Panel({ as: Tag = 'section', title, titleId, action, flush, className, children, ...rest }: PanelProps) {
  const headingId = title ? titleId : undefined;
  return (
    <Tag
      className={['panel', 'glass', flush ? 'panel--flush' : '', className ?? ''].filter(Boolean).join(' ')}
      aria-labelledby={headingId}
      {...rest}
    >
      {(title || action) && (
        <div className="panel__head">
          {title && (
            <h2 className="panel__title" id={headingId}>
              {title}
            </h2>
          )}
          {action}
        </div>
      )}
      {children}
    </Tag>
  );
}
