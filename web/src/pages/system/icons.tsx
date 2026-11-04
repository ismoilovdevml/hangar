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
