import type { SVGProps } from 'react';

type IconProps = SVGProps<SVGSVGElement> & { size?: number };

const s = { stroke: 'currentColor', strokeWidth: 1.7, strokeLinecap: 'round', strokeLinejoin: 'round' } as const;

/** Heartbeat line: System status. */
export const IconPulse = ({ size = 18, ...rest }: IconProps) => (
  <svg width={size} height={size} viewBox="0 0 24 24" fill="none" aria-hidden="true" focusable="false" {...rest}>
    <path d="M3 12h4l2.5-6 4 12 2.5-6H21" {...s} />
  </svg>
);

/** Lines of text in a terminal: System logs. */
export const IconLogs = ({ size = 18, ...rest }: IconProps) => (
  <svg width={size} height={size} viewBox="0 0 24 24" fill="none" aria-hidden="true" focusable="false" {...rest}>
    <rect x="3.5" y="4.5" width="17" height="15" rx="2.5" {...s} />
    <path d="M7.5 9.5h3M7.5 12.5h9M7.5 15.5h6" {...s} />
  </svg>
);

/** The state mark of the System status headline. */
export function StateMark({ tone, size = 26 }: { tone: 'ok' | 'warn' | 'bad' | 'neutral'; size?: number }) {
  const line = { stroke: 'currentColor', strokeWidth: 2.4, strokeLinecap: 'round', strokeLinejoin: 'round' } as const;
  return (
    <svg width={size} height={size} viewBox="0 0 24 24" fill="none" aria-hidden="true" focusable="false">
      {tone === 'ok' && <path d="M5 12l5 5 9-10" {...line} />}
      {tone === 'warn' && <path d="M12 6v7.5M12 18h.01" {...line} />}
      {tone === 'bad' && <path d="M7 7l10 10M17 7L7 17" {...line} />}
      {tone === 'neutral' && <path d="M3 12h4l2.5-6 4 12 2.5-6H21" {...line} strokeWidth={2} />}
    </svg>
  );
}
