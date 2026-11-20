/**
 * The Hangar version the mock server reports. The public demo is built with
 * the release it shows (vite.config.ts sets VITE_DEMO_VERSION from the latest
 * tag), so its sidebar, System status and Cluster pages name a real release;
 * tests and local mock mode use a fixed one.
 */
export const HANGAR_VERSION = import.meta.env.VITE_DEMO_VERSION || '1.0.0';

/** A release before HANGAR_VERSION, for the node that was not upgraded yet: "v0.1.8" → "v0.1.7". */
export const OLDER_VERSION = olderThan(HANGAR_VERSION) ?? '0.9.4';

function olderThan(version: string): string | null {
  if (!import.meta.env.VITE_DEMO_VERSION) return null;
  const m = /^(v?)(\d+)\.(\d+)\.(\d+)$/.exec(version);
  if (!m) return null;
  const v = m[1] ?? '';
  const [major, minor, patch] = [Number(m[2]), Number(m[3]), Number(m[4])];
  if (patch > 0) return `${v}${major}.${minor}.${patch - 1}`;
  if (minor > 0) return `${v}${major}.${minor - 1}.0`;
  return null;
}
