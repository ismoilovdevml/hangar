import { useCallback } from 'react';
import { dismissToast, useCurrentToast, type ToastItem } from '../lib/toast';
import { Toast } from './Toast';

function Item({ item }: { item: ToastItem }) {
  const close = useCallback(() => dismissToast(item.id), [item.id]);
  return (
    <Toast onClose={close}>
      <p>{item.message}</p>
      {item.action && (
        <p>
          <button
            type="button"
            className="button button--secondary button--sm"
            onClick={() => {
              close();
              item.action!.run();
            }}
          >
            {item.action.label}
          </button>
        </p>
      )}
    </Toast>
  );
}

/** Shows the toast raised with toast(); rendered once by the app shell. */
export function Toaster() {
  const item = useCurrentToast();
  return item ? <Item key={item.id} item={item} /> : null;
}
