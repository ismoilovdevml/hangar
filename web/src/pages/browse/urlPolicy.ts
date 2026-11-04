/** Which README links and images the browser may follow or load. */

/** A safe href for a link, or null when the link is shown as text. */
export function safeHref(url: string): string | null {
  const u = url.trim();
  if (u.startsWith('#')) return u;
  try {
    const parsed = new URL(u);
    if (parsed.protocol === 'https:' || parsed.protocol === 'http:' || parsed.protocol === 'mailto:') return parsed.href;
  } catch {
    // relative or malformed: there is no base to resolve it against
  }
  return null;
}

/** Data images the renderer shows inline: raster formats and SVG (an SVG loaded as an image runs no script). */
const DATA_IMAGE = /^data:image\/(png|jpeg|gif|webp|avif|svg\+xml)[;,]/i;

/**
 * A safe image source, or null when the image is replaced by its text:
 * same-origin API paths (the server's signed README image URLs), https URLs
 * (servers that let browsers load README images directly) and data images.
 */
export function safeImageSrc(url: string): string | null {
  const u = url.trim();
  if (u.startsWith('/api/v1/') && !u.startsWith('//')) return u;
  if (DATA_IMAGE.test(u)) return u;
  try {
    const parsed = new URL(u);
    return parsed.protocol === 'https:' ? parsed.href : null;
  } catch {
    return null;
  }
}
