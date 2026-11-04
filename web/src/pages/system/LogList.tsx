import { useLayoutEffect, useMemo, useRef, useState } from 'react';
import { Pill } from '../../components/Pill';
import { clock, inlineAttrs, LEVEL_TONE, offsets, recordJson, rowHeight, visibleRange, type LogRecord } from './model';

/** Viewport height used before layout (and in environments without it). */
const FALLBACK_VIEWPORT = 560;
/** Within this many pixels of the bottom, new records keep the view pinned there. */
const STICK_SLACK = 48;

const recordKey = (r: LogRecord) => `${r.seq}:${r.time}`;

interface LogListProps {
  records: LogRecord[];
  label: string;
}

/**
 * Monospace, virtualised list of log records: only the rows in view are in
 * the DOM. Row heights are computed, not measured (a collapsed row is one
 * line; an open one adds its JSON lines), so scrolling stays exact. While
 * the view is at the bottom it follows new records.
 */
export function LogList({ records, label }: LogListProps) {
  const ref = useRef<HTMLDivElement>(null);
  const [scrollTop, setScrollTop] = useState(0);
  const [viewport, setViewport] = useState(FALLBACK_VIEWPORT);
  const [open, setOpen] = useState<ReadonlySet<string>>(() => new Set());
  const [pinned, setPinned] = useState(true);

  const heights = useMemo(() => records.map((r) => rowHeight(r, open.has(recordKey(r)))), [records, open]);
  const tops = useMemo(() => offsets(heights), [heights]);
  const total = tops[tops.length - 1] ?? 0;

  useLayoutEffect(() => {
    const el = ref.current;
    if (!el) return;
    const measure = () => {
      if (el.clientHeight > 0) setViewport(el.clientHeight);
    };
    measure();
    if (typeof ResizeObserver === 'undefined') return;
    const ro = new ResizeObserver(measure);
    ro.observe(el);
    return () => ro.disconnect();
  }, []);

  // Follow the newest record while pinned to the bottom.
  useLayoutEffect(() => {
    const el = ref.current;
    if (!el || !pinned) return;
    el.scrollTop = Math.max(0, total - (el.clientHeight || viewport));
    setScrollTop(el.scrollTop);
  }, [total, pinned, viewport]);

  const onScroll = () => {
    const el = ref.current;
    if (!el) return;
    setScrollTop(el.scrollTop);
    setPinned(el.scrollHeight - el.scrollTop - el.clientHeight <= STICK_SLACK);
  };

  const toggle = (k: string) =>
    setOpen((prev) => {
      const next = new Set(prev);
      if (next.has(k)) next.delete(k);
      else next.add(k);
      return next;
    });

  const [start, end] = visibleRange(tops, scrollTop, viewport);
  const rows = [];
  for (let i = start; i < end; i++) {
    const r = records[i]!;
    const k = recordKey(r);
    const isOpen = open.has(k);
    const detailId = `log-detail-${r.seq}-${i}`;
    const attrs = inlineAttrs(r.attrs);
    rows.push(
      <li
        key={k}
        className={`log-row log-row--${r.level}${isOpen ? ' is-open' : ''}`}
        style={{ top: tops[i], height: heights[i] }}
        aria-setsize={records.length}
        aria-posinset={i + 1}
      >
        <button type="button" className="log-row__line" aria-expanded={isOpen} aria-controls={isOpen ? detailId : undefined} onClick={() => toggle(k)}>
          <time className="log-row__time" dateTime={r.time}>
            {clock(r.time)}
          </time>
          <Pill tone={LEVEL_TONE[r.level]} className="log-row__level">
            {r.level}
          </Pill>
          <span className="log-row__msg">{r.msg}</span>
          {attrs && <span className="log-row__attrs">{attrs}</span>}
        </button>
        {isOpen && (
          <pre id={detailId} className="log-row__detail" tabIndex={0} aria-label={`Record ${r.seq} as JSON`}>
            {recordJson(r)}
          </pre>
        )}
      </li>,
    );
  }

  return (
    <div className="log-view-wrap">
      <div ref={ref} className="log-view" role="region" aria-label={label} tabIndex={0} onScroll={onScroll}>
        <ul className="log-view__rows" style={{ height: total }} aria-label={`${label}, oldest first`}>
          {rows}
        </ul>
      </div>
      {!pinned && records.length > 0 && (
        <button
          type="button"
          className="button button--secondary button--sm log-view__jump"
          onClick={() => {
            setPinned(true);
          }}
        >
          Jump to newest
        </button>
      )}
    </div>
  );
}
