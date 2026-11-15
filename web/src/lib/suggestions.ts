/** Suggestions for `text`: those starting with it first, then those containing it. */
export function matchSuggestions(options: readonly string[], text: string, limit = 50): string[] {
  const t = text.trim().toLowerCase();
  if (!t) return options.slice(0, limit);
  const starts = options.filter((o) => o.toLowerCase().startsWith(t));
  const contains = options.filter((o) => !o.toLowerCase().startsWith(t) && o.toLowerCase().includes(t));
  // An exact match alone is not worth a popup.
  const out = [...starts, ...contains];
  return out.length === 1 && out[0]!.toLowerCase() === t ? [] : out.slice(0, limit);
}
