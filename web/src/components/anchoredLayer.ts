import { useLayoutEffect, type RefObject } from 'react';

const EDGE = 8; // keep this far from the viewport edges
const GAP = 6; // between the anchor and the layer
const MIN_WIDTH = 180;

/**
 * Keeps a portalled, `position: fixed` layer next to its anchor: below it when
 * there is room, above it otherwise, clamped to the viewport and resized to the
 * space available. Follows scrolling (of any ancestor) and window resizes.
 *
 * Styles are written straight to the layer element, so moving it never
 * re-renders the component.
 */
export function useAnchoredLayer(
  anchorRef: RefObject<HTMLElement | null>,
  layerRef: RefObject<HTMLElement | null>,
  open: boolean,
  maxHeight = 320,
) {
  useLayoutEffect(() => {
    if (!open) return;
    let frame = 0;
    const place = () => {
      frame = 0;
      const anchor = anchorRef.current;
      const layer = layerRef.current;
      if (!anchor || !layer) return;
      const r = anchor.getBoundingClientRect();
      const vw = document.documentElement.clientWidth || window.innerWidth;
      const vh = window.innerHeight;
      const below = vh - r.bottom - GAP - EDGE;
      const above = r.top - GAP - EDGE;
      const up = below < Math.min(maxHeight, 160) && above > below;
      const width = Math.min(Math.max(r.width, MIN_WIDTH), vw - 2 * EDGE);
      const left = Math.max(EDGE, Math.min(r.left, vw - EDGE - width));
      const s = layer.style;
      s.left = `${left}px`;
      s.minWidth = `${width}px`;
      s.maxWidth = `${vw - left - EDGE}px`;
      s.maxHeight = `${Math.max(120, Math.min(maxHeight, up ? above : below))}px`;
      if (up) {
        s.top = '';
        s.bottom = `${vh - r.top + GAP}px`;
      } else {
        s.bottom = '';
        s.top = `${r.bottom + GAP}px`;
      }
      layer.dataset.placement = up ? 'above' : 'below';
    };
    const schedule = () => {
      if (!frame) frame = requestAnimationFrame(place);
    };
    place();
    window.addEventListener('scroll', schedule, true);
    window.addEventListener('resize', schedule);
    return () => {
      if (frame) cancelAnimationFrame(frame);
      window.removeEventListener('scroll', schedule, true);
      window.removeEventListener('resize', schedule);
    };
  }, [anchorRef, layerRef, open, maxHeight]);
}

/** Layers opened from inside a modal dialog must stack above the dialog's scrim. */
export const insideModal = (el: HTMLElement | null) => !!el?.closest('[aria-modal="true"]');
