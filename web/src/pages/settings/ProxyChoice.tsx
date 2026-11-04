/**
 * The outbound proxy of a proxy repository: the platform default, a named
 * proxy, or a direct connection. Used by the repository forms.
 */
import { Select, type SelectOption } from '../../components/Select';
import { hostPort } from './model';
import { useProxies } from './settingsApi';

/** inherit = platform default; named = proxyName; none = direct; url = a proxy URL stored before named proxies. */
export type ProxyChoiceMode = 'inherit' | 'named' | 'none' | 'url';

interface ProxyChoiceProps {
  id: string;
  mode: ProxyChoiceMode;
  proxyName: string;
  onChange: (mode: ProxyChoiceMode, proxyName: string) => void;
  describedBy?: string;
  invalid?: boolean;
  disabled?: boolean;
}

export function ProxyChoice({ id, mode, proxyName, onChange, describedBy, invalid, disabled }: ProxyChoiceProps) {
  const q = useProxies();
  const list = q.data;
  const def = list?.items.find((p) => p.name === list.default);
  const defaultHint = def
    ? `Currently ${def.name}${list?.limited ? '' : ` (${hostPort(def.host, def.port)})`}`
    : list?.environment_proxy
      ? `No default proxy: ${list.environment_proxy} from the server environment`
      : list
        ? 'No default proxy: direct connection'
        : undefined;

  const options: SelectOption[] = [{ value: 'inherit', label: 'Platform default', hint: defaultHint }];
  for (const p of list?.items ?? []) {
    options.push({ value: `named:${p.name}`, label: p.name, hint: list?.limited ? undefined : `${p.scheme}://${hostPort(p.host, p.port)}` });
  }
  if (mode === 'named' && proxyName && !list?.items.some((p) => p.name === proxyName)) {
    options.push({ value: `named:${proxyName}`, label: proxyName, hint: q.isError ? undefined : 'This proxy no longer exists' });
  }
  options.push({ value: 'none', label: 'No proxy (direct)' });
  if (mode === 'url') options.push({ value: 'url', label: 'Custom proxy URL', hint: 'Set before named proxies; pick a named proxy instead' });

  const value = mode === 'named' ? `named:${proxyName}` : mode;
  return (
    <Select
      id={id}
      value={value}
      options={options}
      aria-describedby={describedBy}
      aria-invalid={invalid || undefined}
      disabled={disabled}
      onChange={(v) => {
        if (v.startsWith('named:')) onChange('named', v.slice('named:'.length));
        else onChange(v as ProxyChoiceMode, '');
      }}
    />
  );
}
