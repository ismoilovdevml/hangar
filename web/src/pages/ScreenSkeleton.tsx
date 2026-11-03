import type { ReactNode } from 'react';
import { PageHeader } from '../components/PageHeader';
import { Panel } from '../components/Panel';

interface ScreenSkeletonProps {
  title: ReactNode;
  description: ReactNode;
  actions?: ReactNode;
  /** What the finished screen will show, in plain language. */
  planned: string[];
  children?: ReactNode;
}

/** Real page frame for screens whose body is still being built. */
export function ScreenSkeleton({ title, description, actions, planned, children }: ScreenSkeletonProps) {
  return (
    <div className="screen">
      <PageHeader title={title} description={description} actions={actions} />
      {children}
      <Panel title="This screen is in progress" titleId="in-progress-title" className="in-progress">
        <p className="in-progress__lead">The page frame and navigation work; the content below is not wired up yet. It will show:</p>
        <ul className="in-progress__list">
          {planned.map((p) => (
            <li key={p}>{p}</li>
          ))}
        </ul>
      </Panel>
    </div>
  );
}
