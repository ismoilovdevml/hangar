import { useState, type ReactNode } from 'react';
import { Dialog } from './ui';

/**
 * Closing a dialog that shows a new secret: until "I have stored the token"
 * is ticked, the close button, Escape and the scrim ask first, so the only
 * copy is never thrown away by accident.
 */
export function useSecretCloseGuard(revealed: boolean, onClose: () => void) {
  const [stored, setStored] = useState(false);
  const [asking, setAsking] = useState(false);
  const requestClose = () => (revealed && !stored ? setAsking(true) : onClose());
  const confirm: ReactNode = asking ? (
    <Dialog title="Close without the token?" onClose={() => setAsking(false)}>
      <div className="dialog-form">
        <p className="dialog__text">This is the only time the secret is shown. Once closed it cannot be shown again; you would have to create a new token.</p>
        <div className="dialog-actions">
          <button type="button" className="button button--secondary" onClick={onClose}>
            Close anyway
          </button>
          <button type="button" className="button button--primary" data-autofocus onClick={() => setAsking(false)}>
            Keep it open
          </button>
        </div>
      </div>
    </Dialog>
  ) : null;
  return { stored, setStored, requestClose, confirm };
}
