import type { SVGProps } from 'react';

type IconProps = SVGProps<SVGSVGElement> & { size?: number };

const s = { stroke: 'currentColor', strokeWidth: 1.7, strokeLinecap: 'round', strokeLinejoin: 'round' } as const;

/** A padlock: sign-in settings. */
export const IconLock = ({ size = 18, ...p }: IconProps) => (
  <svg width={size} height={size} viewBox="0 0 24 24" fill="none" aria-hidden="true" focusable="false" {...p}>
    <rect x="4.5" y="10.5" width="15" height="10" rx="2.5" {...s} />
    <path d="M8 10.5V7.5a4 4 0 0 1 8 0v3" {...s} />
    <path d="M12 14.5v2" {...s} />
  </svg>
);

/** A route through a waypoint: outbound proxies. */
export const IconRoute = ({ size = 18, ...p }: IconProps) => (
  <svg width={size} height={size} viewBox="0 0 24 24" fill="none" aria-hidden="true" focusable="false" {...p}>
    <circle cx="5" cy="6" r="2" {...s} />
    <circle cx="19" cy="18" r="2" {...s} />
    <rect x="9.5" y="9.5" width="5" height="5" rx="1.2" {...s} />
    <path d="M7 6h3.5a1.5 1.5 0 0 1 1.5 1.5v2M12 14.5v2a1.5 1.5 0 0 0 1.5 1.5H17" {...s} />
  </svg>
);
