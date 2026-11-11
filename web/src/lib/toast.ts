import { useSyncExternalStore } from 'react';

/** A button inside a toast, e.g. Undo. */
export interface ToastAction {
  label: string;
  run: () => void;
}

export interface ToastItem {
  id: number;
  message: string;
  action?: ToastAction;
}

let current: ToastItem | null = null;
let nextId = 1;
const listeners = new Set<() => void>();
const emit = () => listeners.forEach((l) => l());

/**
 * Confirms a finished write at the bottom of the screen ("Deleted npm-proxy").
 * One toast at a time: a newer one replaces the last.
 */
export function toast(message: string, action?: ToastAction): void {
  current = { id: nextId++, message, action };
  emit();
}

export function dismissToast(id: number): void {
  if (current?.id !== id) return;
  current = null;
  emit();
}

/** Drops any toast (tests start each case without one). */
export function clearToasts(): void {
  current = null;
  emit();
}

function subscribe(l: () => void) {
  listeners.add(l);
  return () => listeners.delete(l);
}

export function useCurrentToast(): ToastItem | null {
  return useSyncExternalStore(subscribe, () => current, () => null);
}
