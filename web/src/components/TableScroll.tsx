import { useLayoutEffect, useRef, type ReactNode } from 'react';

/**
 * How readily a column gives way when its table does not fit the panel:
 * 1 never hides (the name, the status and the row actions), 3 hides first,
 * then 2. Cells carry it as `data-priority`; on phones level 3 also drops
 * out of the stacked cards.
 */
export type ColumnPriority = 1 | 2 | 3;

/** Hides levels 3, then 2, until the table fits; then marks the edges that still hide columns. */
function fit(scroller: HTMLElement, frame: HTMLElement) {
  delete scroller.dataset.hide;
  for (const level of ['3', '2']) {
    if (scroller.scrollWidth <= scroller.clientWidth + 1) break;
    // `data-hide="2"` hides levels 2 and 3.
    if (scroller.querySelector(`[data-priority="${level}"]`)) scroller.dataset.hide = level;
  }
  edges(scroller, frame);
}

/** A shadow on each side that has columns scrolled out of view. */
function edges(scroller: HTMLElement, frame: HTMLElement) {
  const rest = scroller.scrollWidth - scroller.clientWidth - scroller.scrollLeft;
  frame.toggleAttribute('data-more-left', scroller.scrollLeft > 1);
  frame.toggleAttribute('data-more-right', rest > 1);
}

/**
 * The scroll box around a table inside a panel. Columns marked with a
 * `data-priority` give way before the table scrolls sideways, and when it
 * still has to scroll, a shadow shows on the side with more columns.
 */
export function TableScroll({ label, children }: { label: string; children: ReactNode }) {
  const frameRef = useRef<HTMLDivElement>(null);
  const scrollRef = useRef<HTMLDivElement>(null);

  useLayoutEffect(() => {
    const scroller = scrollRef.current;
    const frame = frameRef.current;
    if (!scroller || !frame) return;
    fit(scroller, frame);
    let width = scroller.clientWidth;
    let live = true;
    const onScroll = () => edges(scroller, frame);
    scroller.addEventListener('scroll', onScroll, { passive: true });
    // Fit again when rows or cells change, not on every render of the page
    // around the table (a poll that changes nothing then costs no layout).
    // Mutation callbacks run before the browser paints.
    const mo = new MutationObserver(() => fit(scroller, frame));
    mo.observe(scroller, { childList: true, subtree: true, characterData: true, attributes: true, attributeFilter: ['hidden', 'data-priority'] });
    const ro =
      typeof ResizeObserver === 'undefined'
        ? null
        : new ResizeObserver(() => {
            if (scroller.clientWidth === width) return;
            width = scroller.clientWidth;
            fit(scroller, frame);
          });
    ro?.observe(scroller);
    // Web fonts change column widths once they arrive.
    void document.fonts?.ready.then(() => live && fit(scroller, frame));
    return () => {
      live = false;
      scroller.removeEventListener('scroll', onScroll);
      mo.disconnect();
      ro?.disconnect();
    };
  }, []);

  return (
    <div className="table-frame" ref={frameRef}>
      {/* The scroll box is focusable so keyboards can scroll it; its name differs from the panel around it, which often shares the caption. */}
      <div className="table-scroll" ref={scrollRef} tabIndex={0} role="region" aria-label={label}>
        {children}
      </div>
    </div>
  );
}
