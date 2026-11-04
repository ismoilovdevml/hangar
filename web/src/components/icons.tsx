import type { SVGProps } from 'react';

type IconProps = SVGProps<SVGSVGElement> & { size?: number };

function Svg({ size = 18, children, ...rest }: IconProps) {
  return (
    <svg width={size} height={size} viewBox="0 0 24 24" fill="none" aria-hidden="true" focusable="false" {...rest}>
      {children}
    </svg>
  );
}

const s = { stroke: 'currentColor', strokeWidth: 1.7 } as const;

export const IconLogo = (p: IconProps) => (
  <Svg {...p}>
    <path d="M3 20V9l9-5 9 5v11" stroke="currentColor" strokeWidth="2" strokeLinejoin="round" />
    <path d="M8 20v-6h8v6" stroke="currentColor" strokeWidth="2" />
  </Svg>
);
export const IconOverview = (p: IconProps) => (
  <Svg {...p}>
    <rect x="3" y="3" width="7" height="9" rx="2" {...s} />
    <rect x="14" y="3" width="7" height="5" rx="2" {...s} />
    <rect x="14" y="12" width="7" height="9" rx="2" {...s} />
    <rect x="3" y="16" width="7" height="5" rx="2" {...s} />
  </Svg>
);
export const IconPackage = (p: IconProps) => (
  <Svg {...p}>
    <path d="M12 3l8 4.5v9L12 21l-8-4.5v-9L12 3z" {...s} strokeLinejoin="round" />
    <path d="M4 7.5l8 4.5 8-4.5M12 12v9" {...s} />
  </Svg>
);
export const IconSearch = (p: IconProps) => (
  <Svg {...p}>
    <circle cx="11" cy="11" r="7" {...s} />
    <path d="M20 20l-3.5-3.5" {...s} strokeLinecap="round" />
  </Svg>
);
export const IconDatabase = (p: IconProps) => (
  <Svg {...p}>
    <ellipse cx="12" cy="6" rx="8" ry="3" {...s} />
    <path d="M4 6v6c0 1.7 3.6 3 8 3s8-1.3 8-3V6M4 12v6c0 1.7 3.6 3 8 3s8-1.3 8-3v-6" {...s} />
  </Svg>
);
export const IconServer = (p: IconProps) => (
  <Svg {...p}>
    <rect x="3" y="4" width="18" height="7" rx="2" {...s} />
    <rect x="3" y="13" width="18" height="7" rx="2" {...s} />
    <circle cx="7" cy="7.5" r="1" fill="currentColor" />
    <circle cx="7" cy="16.5" r="1" fill="currentColor" />
  </Svg>
);
export const IconTrash = (p: IconProps) => (
  <Svg {...p}>
    <path d="M4 7h16M9 7V4h6v3M6 7l1 13h10l1-13" {...s} strokeLinejoin="round" />
  </Svg>
);
export const IconShield = (p: IconProps) => (
  <Svg {...p}>
    <path d="M12 3l7 3v6c0 4.5-3 7.7-7 9-4-1.3-7-4.5-7-9V6l7-3z" {...s} strokeLinejoin="round" />
    <path d="M9 12l2 2 4-4" {...s} />
  </Svg>
);
export const IconUsers = (p: IconProps) => (
  <Svg {...p}>
    <circle cx="9" cy="8" r="3.5" {...s} />
    <path d="M3 20c0-3.3 2.7-6 6-6s6 2.7 6 6M16 4.5a3.5 3.5 0 010 7M18 14c1.8.8 3 2.6 3 6" {...s} strokeLinecap="round" />
  </Svg>
);
export const IconClock = (p: IconProps) => (
  <Svg {...p}>
    <circle cx="12" cy="12" r="8.5" {...s} />
    <path d="M12 7.5V12l3 2" {...s} strokeLinecap="round" />
  </Svg>
);
export const IconCluster = (p: IconProps) => (
  <Svg {...p}>
    <circle cx="12" cy="5" r="2.5" {...s} />
    <circle cx="5" cy="18" r="2.5" {...s} />
    <circle cx="19" cy="18" r="2.5" {...s} />
    <path d="M10.8 7.2L6.2 15.8M13.2 7.2l4.6 8.6M7.5 18h9" {...s} />
  </Svg>
);
export const IconBell = (p: IconProps) => (
  <Svg {...p}>
    <path d="M6 16V11a6 6 0 1112 0v5l1.5 1.5h-15L6 16zM10 20.5h4" {...s} strokeLinecap="round" />
  </Svg>
);
export const IconChevronUpDown = (p: IconProps) => (
  <svg width={p.size ?? 14} height={p.size ?? 14} viewBox="0 0 14 14" fill="none" aria-hidden="true" focusable="false">
    <path d="M4 5.5l3-3 3 3M4 8.5l3 3 3-3" stroke="currentColor" strokeWidth="1.5" />
  </svg>
);
export const IconMenu = (p: IconProps) => (
  <Svg {...p}>
    <path d="M4 7h16M4 12h16M4 17h16" {...s} strokeLinecap="round" />
  </Svg>
);
export const IconClose = (p: IconProps) => (
  <Svg {...p}>
    <path d="M6 6l12 12M18 6L6 18" {...s} strokeLinecap="round" />
  </Svg>
);
export const IconCopy = (p: IconProps) => (
  <Svg {...p}>
    <rect x="8" y="8" width="12" height="12" rx="2.5" {...s} />
    <path d="M16 8V6.5A2.5 2.5 0 0013.5 4h-7A2.5 2.5 0 004 6.5v7A2.5 2.5 0 006.5 16H8" {...s} />
  </Svg>
);
export const IconCheck = (p: IconProps) => (
  <Svg {...p}>
    <path d="M5 12.5l4.5 4.5L19 7.5" {...s} strokeLinecap="round" strokeLinejoin="round" />
  </Svg>
);
export const IconPlus = (p: IconProps) => (
  <Svg {...p}>
    <path d="M12 5v14M5 12h14" {...s} strokeLinecap="round" />
  </Svg>
);
export const IconArrowRight = (p: IconProps) => (
  <Svg {...p}>
    <path d="M5 12h14M13 6l6 6-6 6" {...s} strokeLinecap="round" strokeLinejoin="round" />
  </Svg>
);
export const IconLogout = (p: IconProps) => (
  <Svg {...p}>
    <path d="M10 4H6.5A2.5 2.5 0 004 6.5v11A2.5 2.5 0 006.5 20H10M15 8l4 4-4 4M19 12H9" {...s} strokeLinecap="round" strokeLinejoin="round" />
  </Svg>
);
export const IconGlobe = (p: IconProps) => (
  <Svg {...p}>
    <circle cx="12" cy="12" r="8.5" {...s} />
    <path d="M3.5 12h17M12 3.5c2.3 2.4 3.5 5.2 3.5 8.5s-1.2 6.1-3.5 8.5c-2.3-2.4-3.5-5.2-3.5-8.5s1.2-6.1 3.5-8.5z" {...s} />
  </Svg>
);
export const IconGroup = (p: IconProps) => (
  <Svg {...p}>
    <rect x="3.5" y="4" width="7" height="6" rx="2" {...s} />
    <rect x="13.5" y="4" width="7" height="6" rx="2" {...s} />
    <rect x="8.5" y="14" width="7" height="6" rx="2" {...s} />
    <path d="M7 10v2h10v-2M12 12v2" {...s} />
  </Svg>
);
export const IconArrowUp = (p: IconProps) => (
  <Svg {...p}>
    <path d="M12 19V5M6 11l6-6 6 6" {...s} strokeLinecap="round" strokeLinejoin="round" />
  </Svg>
);
export const IconArrowDown = (p: IconProps) => (
  <Svg {...p}>
    <path d="M12 5v14M6 13l6 6 6-6" {...s} strokeLinecap="round" strokeLinejoin="round" />
  </Svg>
);
