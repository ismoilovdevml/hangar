import type { UnsavedChangesBlocker } from '../lib/unsavedChanges';
import { ConfirmDialog } from './ConfirmDialog';

/** "Leave without saving?" while useUnsavedChangesGuard holds a navigation back. */
export function UnsavedChangesDialog({ blocker, what = 'your unsaved changes' }: { blocker: UnsavedChangesBlocker; what?: string }) {
  if (blocker.status !== 'blocked') return null;
  return (
    <ConfirmDialog title="Leave without saving?" confirmLabel="Leave" cancelLabel="Stay" tone="danger" onConfirm={blocker.proceed} onClose={blocker.reset}>
      <p>Leaving now discards {what}.</p>
    </ConfirmDialog>
  );
}
