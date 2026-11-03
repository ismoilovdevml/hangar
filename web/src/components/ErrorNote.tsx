export function ErrorNote({ what, onRetry }: { what: string; onRetry: () => void }) {
  return (
    <div className="error-note" role="alert">
      <span>Could not load {what}.</span>
      <button type="button" className="button button--secondary button--sm" onClick={onRetry}>
        Retry
      </button>
    </div>
  );
}
