import { useId, useState, type FormEvent, type ReactNode } from 'react';
import { errorText } from '../lib/apiError';
import { Dialog } from './Dialog';
import { ErrorMessage } from './States';

interface ConfirmDialogProps {
  title: string;
  children: ReactNode;
  confirmLabel: string;
  /** The label of the button that backs out; "Cancel" by default. */
  cancelLabel?: string;
  /** Destructive actions get a red confirm button; Cancel is focused first either way. */
  tone?: 'danger' | 'primary';
  /** The caller runs the action itself (a mutation's `isPending`). */
  busy?: boolean;
  /** The caller's sentence for a failed action. */
  error?: string | null;
  /** When set, the user must type this exact text (e.g. the name) before confirming. */
  typeToConfirm?: string;
  /**
   * Runs the action. A returned promise is awaited here: the dialog shows
   * it as busy, closes when it resolves, and stays open with the error when
   * it rejects. Without one the caller closes the dialog.
   */
  onConfirm: () => void | Promise<unknown>;
  /** Cancel, Escape, the scrim, or an awaited action that succeeded. */
  onClose: () => void;
}

/**
 * Confirmation for irreversible or expensive actions. Render it only while
 * open; it is a Dialog (focus trap, Escape, focus returns on close) that
 * cannot be dismissed while the action runs.
 */
export function ConfirmDialog({
  title,
  children,
  confirmLabel,
  cancelLabel = 'Cancel',
  tone = 'primary',
  busy,
  error,
  typeToConfirm,
  onConfirm,
  onClose,
}: ConfirmDialogProps) {
  const inputId = useId();
  const [typed, setTyped] = useState('');
  const [running, setRunning] = useState(false);
  const [failure, setFailure] = useState<unknown>(null);
  const working = busy || running;
  const ready = !typeToConfirm || typed === typeToConfirm;

  const submit = (e: FormEvent) => {
    e.preventDefault();
    if (working || !ready) return;
    const result = onConfirm();
    if (!(result instanceof Promise)) return;
    setRunning(true);
    setFailure(null);
    result.then(
      () => onClose(),
      (err: unknown) => {
        setFailure(err);
        setRunning(false);
      },
    );
  };

  return (
    <Dialog title={title} onClose={onClose} variant="plain" role="alertdialog" dismissible={!working}>
      <form className="dialog-form" onSubmit={submit}>
        <div className="dialog__text">{children}</div>
        {typeToConfirm && (
          <div className="field">
            <label htmlFor={inputId} className="field__label">
              Type <code className="inline-code">{typeToConfirm}</code> to confirm
            </label>
            <input
              id={inputId}
              className="input input--mono"
              autoComplete="off"
              autoCapitalize="off"
              spellCheck={false}
              value={typed}
              onChange={(e) => setTyped(e.target.value)}
              data-autofocus
            />
          </div>
        )}
        {error ? (
          <p className="form-error" role="alert">
            {error}
          </p>
        ) : (
          failure !== null && <ErrorMessage error={failure} message={errorText(failure)} />
        )}
        <div className="dialog__actions">
          <button type="button" className="button button--secondary" onClick={onClose} disabled={working} data-autofocus={typeToConfirm ? undefined : true}>
            {cancelLabel}
          </button>
          <button
            type="submit"
            className={tone === 'danger' ? 'button button--danger' : 'button button--primary'}
            disabled={working || !ready}
            aria-busy={working || undefined}
          >
            {working ? 'Working…' : confirmLabel}
          </button>
        </div>
      </form>
    </Dialog>
  );
}
