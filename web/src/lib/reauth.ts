import { useSyncExternalStore } from 'react';

/**
 * The session ended while the app was open (it expired or was revoked).
 * Instead of leaving the page, the shell asks for the password in place, so
 * whatever the user was editing is still there afterwards.
 */
let username: string | null = null;
const listeners = new Set<() => void>();
const emit = () => listeners.forEach((l) => l());

/** Asks the signed-out user (by name) to sign in again where they are. */
export function requestReauth(name: string): void {
  if (username === name) return;
  username = name;
  emit();
}

export function endReauth(): void {
  if (username === null) return;
  username = null;
  emit();
}

/** The user asked to sign in again, or null. */
export function useReauth(): string | null {
  return useSyncExternalStore(
    (l) => {
      listeners.add(l);
      return () => listeners.delete(l);
    },
    () => username,
    () => null,
  );
}
