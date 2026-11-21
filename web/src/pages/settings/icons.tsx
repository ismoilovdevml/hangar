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

/** A hook through a node: outgoing webhooks. */
export const IconWebhook = ({ size = 18, ...p }: IconProps) => (
  <svg width={size} height={size} viewBox="0 0 24 24" fill="none" aria-hidden="true" focusable="false" {...p}>
    <circle cx="12" cy="6.5" r="2.5" {...s} />
    <circle cx="6" cy="17" r="2.5" {...s} />
    <circle cx="18" cy="17" r="2.5" {...s} />
    <path d="M10.8 8.7 7.3 14.8M13.2 8.7l3.5 6.1M8.5 17h7" {...s} />
  </svg>
);

/** A badge with a check: CI jobs trusted by their platform's ID token. */
export const IconBadgeCheck = ({ size = 18, ...p }: IconProps) => (
  <svg width={size} height={size} viewBox="0 0 24 24" fill="none" aria-hidden="true" focusable="false" {...p}>
    <path d="M12 3.5 18.5 6v5.5c0 4-2.8 7.2-6.5 8.5-3.7-1.3-6.5-4.5-6.5-8.5V6L12 3.5Z" {...s} />
    <path d="m9 12 2.2 2.2L15.5 10" {...s} />
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
