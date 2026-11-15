/**
 * The password just typed on the sign-in form, kept in memory only (never in
 * storage, the URL or history state) so the forced password change right
 * after does not ask for it again. It is forgotten once used, on sign-out,
 * after a few minutes, or when the page reloads.
 */
let held: { password: string; at: number } | null = null;
const KEEP_MS = 10 * 60_000;

export function holdSignInPassword(password: string): void {
  held = { password, at: Date.now() };
}

export function peekSignInPassword(): string | null {
  if (held && Date.now() - held.at > KEEP_MS) held = null;
  return held?.password ?? null;
}

export function forgetSignInPassword(): void {
  held = null;
}
