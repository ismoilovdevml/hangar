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

/** A safe image source (https only), or null when the image is replaced by its text. */
export function safeImageSrc(url: string): string | null {
  try {
    const parsed = new URL(url.trim());
    return parsed.protocol === 'https:' ? parsed.href : null;
  } catch {
    return null;
  }
}

