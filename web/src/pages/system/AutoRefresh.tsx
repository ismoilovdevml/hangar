import { IconPlay, IconRefresh } from '../../components/opsIcons';
import { useNow } from '../../lib/useNow';

interface AutoRefreshProps {
  /** When the data was last received (ms since epoch). */
  updatedAt?: number;
  intervalMs: number;
  paused: boolean;
  fetching: boolean;
  onPause: (paused: boolean) => void;
  onRefresh: () => void;
  /** What is refreshed, for the button names ("status", "logs"). */
  what: string;
}

function IconPause({ size = 16 }: { size?: number }) {
  return (
    <svg width={size} height={size} viewBox="0 0 24 24" fill="none" aria-hidden="true" focusable="false">
      <path d="M9 6v12M15 6v12" stroke="currentColor" strokeWidth={1.9} strokeLinecap="round" />
    </svg>
  );
}

/** "Refreshing in 9 s" with Pause/Resume and Refresh now. */
export function AutoRefresh({ updatedAt, intervalMs, paused, fetching, onPause, onRefresh, what }: AutoRefreshProps) {
  const now = useNow(1_000);
  const left = updatedAt ? Math.max(0, Math.ceil((updatedAt + intervalMs - now.getTime()) / 1000)) : null;
  const text = fetching ? 'Refreshing…' : paused ? 'Auto-refresh paused' : left === null ? ' ' : left === 0 ? 'Refreshing…' : `Refreshing in ${left} s`;
  return (
    <div className="auto-refresh" role="group" aria-label={`Refresh ${what}`}>
      <span className="auto-refresh__text" aria-live="off">
        {!paused && <span className="status__dot dot-ok auto-refresh__pulse" aria-hidden="true" />}
        {text}
      </span>
      <button type="button" className="button button--secondary button--sm" onClick={() => onPause(!paused)}>
        {paused ? <IconPlay size={16} /> : <IconPause />}
        {paused ? 'Resume' : 'Pause'}
      </button>
      <button type="button" className="button button--secondary button--sm" onClick={onRefresh} disabled={fetching}>
        <IconRefresh size={16} />
        Refresh now
      </button>
    </div>
  );
}
