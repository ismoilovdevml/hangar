import type { ReactNode } from 'react';
import type { Tone } from '../lib/format';

export function Pill({ tone = 'neutral', children, className }: { tone?: Tone; children: ReactNode; className?: string }) {
  return <span className={['pill', `tone-${tone}`, className ?? ''].filter(Boolean).join(' ')}>{children}</span>;
}

/** A coloured dot with its meaning always spelled out next to it. */
export function StatusDot({ tone, label, halo, title }: { tone: 'ok' | 'warn' | 'bad' | 'neutral'; label?: ReactNode; halo?: boolean; title?: string }) {
  return (
    <span className="status" title={title}>
      <span className={['status__dot', `dot-${tone}`, halo ? 'status__dot--halo' : ''].join(' ')} aria-hidden="true" />
      {label}
    </span>
  );
}
