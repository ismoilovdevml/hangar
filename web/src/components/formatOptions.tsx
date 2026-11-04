import type { Format } from '../api/types';
import { FORMAT_META } from '../lib/format';
import { FormatIcon } from './FormatIcon';
import type { SelectOption } from './Select';

/** Select options for formats, each with its mark; `any` adds a first "all formats" choice. */
export function formatOptions(formats: readonly Format[], any?: string): SelectOption<Format | ''>[] {
  const list: SelectOption<Format | ''>[] = formats.map((f) => ({
    value: f,
    label: FORMAT_META[f].label,
    icon: <FormatIcon format={f} size={16} label={null} />,
  }));
  return any === undefined ? list : [{ value: '', label: any }, ...list];
}
