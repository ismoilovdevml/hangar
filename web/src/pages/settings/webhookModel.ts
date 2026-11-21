/** Pure helpers of the Webhooks page: the edit form, its checks and the wording of states. */
import { cleanServerMessage } from '../../lib/apiError';
import type { Tone } from '../../lib/format';
import { pathMatch } from '../../lib/privileges';
import { fieldErrorsOf } from './settingsApi';
import type { DeliveryState, Webhook, WebhookDelivery, WebhookEvent, WebhookFormat, WebhookInput } from './webhooksApi';

export const WEBHOOK_EVENTS: { value: WebhookEvent; label: string; hint: string }[] = [
  { value: 'component.published', label: 'Component published', hint: 'Uploaded, or cached by a proxy' },
  { value: 'component.deleted', label: 'Component deleted', hint: 'By a user, a cleanup policy or Docker GC' },
  { value: 'component.quarantined', label: 'Component quarantined', hint: 'The quarantine policy blocked it' },
  { value: 'component.released', label: 'Component released', hint: 'It left quarantine' },
  { value: 'repository.created', label: 'Repository created', hint: '' },
  { value: 'repository.deleted', label: 'Repository deleted', hint: '' },
  { value: 'scan.finding', label: 'New vulnerabilities', hint: 'A scan found findings new to a component' },
];

export const WEBHOOK_FORMATS: { value: WebhookFormat; label: string; hint: string }[] = [
  { value: 'generic', label: 'JSON', hint: 'The event envelope, for your own receiver' },
  { value: 'slack', label: 'Slack', hint: 'A Slack incoming webhook URL' },
  { value: 'teams', label: 'Microsoft Teams', hint: 'A Teams Workflows or incoming webhook URL' },
];

export const formatLabel = (f: WebhookFormat) => WEBHOOK_FORMATS.find((x) => x.value === f)?.label ?? f;
export const eventLabel = (e: string) => (e === 'ping' ? 'Test ping' : (WEBHOOK_EVENTS.find((x) => x.value === e)?.label ?? e));

/** What happens to the stored secret on save. */
export type SecretChoice = 'keep' | 'replace' | 'clear';

export interface WebhookForm {
  name: string;
  /** The new URL; on edit "" keeps the stored one. */
  url: string;
  /** Edit: the user chose to replace the stored URL. */
  replaceUrl: boolean;
  format: WebhookFormat;
  events: WebhookEvent[];
  repositories: string[];
  secretChoice: SecretChoice;
  secret: string;
  enabled: boolean;
}

export const NEW_WEBHOOK: WebhookForm = {
  name: '',
  url: '',
  replaceUrl: true,
  format: 'generic',
  events: ['component.published'],
  repositories: [],
  secretChoice: 'replace',
  secret: '',
  enabled: true,
};

export function webhookForm(h: Webhook): WebhookForm {
  return {
    name: h.name,
    url: '',
    replaceUrl: false,
    format: h.format,
    events: [...h.events],
    repositories: [...h.repositories],
    secretChoice: 'keep',
    secret: '',
    enabled: h.enabled,
  };
}

export type WebhookErrors = Partial<Record<'name' | 'url' | 'format' | 'events' | 'repositories' | 'secret', string>>;

const NAME_RE = /^[A-Za-z0-9][A-Za-z0-9 ._-]{0,63}$/;
/** A repository name or glob as the server accepts it: the privilege pattern alphabet (internal/webhooks repoPatternRE), and a valid path.Match pattern. */
const REPO_PATTERN_RE = /^[a-z0-9*?[\]^!-]{1,64}$/;
const validRepoPattern = (p: string) => REPO_PATTERN_RE.test(p) && !pathMatch(p, '').error;

/** What is wrong with an http(s) URL the user typed; null when it is fine. */
export function urlError(raw: string): string | null {
  const s = raw.trim();
  if (!s) return 'Enter the address the events are sent to.';
  let u: URL;
  try {
    u = new URL(s);
  } catch {
    return 'Enter an absolute http:// or https:// URL.';
  }
  if (u.protocol !== 'http:' && u.protocol !== 'https:') return 'Enter an absolute http:// or https:// URL.';
  if (u.username || u.password) return 'Leave the user name and password out of the URL; use the secret to authenticate deliveries.';
  if (u.hash) return 'Leave out the fragment (#…).';
  if (s.includes('/***')) return 'This is the shown, shortened URL. Paste the full URL, or keep the stored one.';
  return null;
}

/** Problems the server would refuse, found before sending. `stored` is the webhook being edited. */
export function validateWebhook(f: WebhookForm, stored: Webhook | null): WebhookErrors {
  const e: WebhookErrors = {};
  if (!NAME_RE.test(f.name.trim())) e.name = 'Use 1 to 64 letters, digits, spaces, ".", "_" or "-", starting with a letter or digit.';
  if (!stored || f.replaceUrl) {
    const u = urlError(f.url);
    if (u) e.url = u;
  }
  if (f.events.length === 0) e.events = 'Choose at least one event.';
  if (f.repositories.length > 100) e.repositories = 'Name at most 100 repositories or patterns.';
  const bad = f.repositories.find((r) => !validRepoPattern(r));
  if (bad) e.repositories = `"${bad}": enter a repository name or a glob such as npm-*.`;
  if (f.secretChoice === 'replace' && stored?.secret_configured && !f.secret) e.secret = 'Enter the new secret, or keep the stored one.';
  return e;
}

/** The request body. On edit an empty url and secret keep what is stored; `enabled` is always sent. */
export function webhookInput(f: WebhookForm, stored: Webhook | null): WebhookInput {
  const body: WebhookInput = {
    name: f.name.trim(),
    events: [...f.events],
    repositories: [...f.repositories],
    format: f.format,
    enabled: f.enabled,
  };
  if (!stored || f.replaceUrl) body.url = f.url.trim();
  if (f.secretChoice === 'clear') body.clear_secret = true;
  else if (f.secretChoice === 'replace' && f.secret) body.secret = f.secret;
  return body;
}

/** Scheme, host and port of a URL ("https://hooks.slack.com:443"), default ports explicit; null when it is not one. */
export function endpointOf(raw: string): string | null {
  try {
    const u = new URL(raw.replace(/\/\*\*\*$/, ''));
    if (!u.host) return null;
    const port = u.port || (u.protocol === 'https:' ? '443' : u.protocol === 'http:' ? '80' : '');
    return `${u.protocol}//${u.hostname.toLowerCase()}:${port}`;
  } catch {
    return null;
  }
}

/**
 * Whether saving would send the stored secret to another endpoint: the
 * server refuses that unless the secret is entered again or removed.
 */
export function secretMustBeRenewed(f: WebhookForm, stored: Webhook | null): boolean {
  if (!stored?.secret_configured || !f.replaceUrl || f.secretChoice !== 'keep') return false;
  const next = endpointOf(f.url.trim());
  return next !== null && next !== endpointOf(stored.url);
}

/** The state of a webhook as one pill. */
export function webhookStatus(h: Webhook): { label: string; tone: Tone } {
  if (h.enabled) return h.consecutive_failures > 0 ? { label: 'Failing', tone: 'warn' } : { label: 'Enabled', tone: 'ok' };
  return h.disabled_reason ? { label: 'Disabled by the server', tone: 'bad' } : { label: 'Disabled', tone: 'neutral' };
}

/** A receiver's HTTP status in words for the last-delivery column. */
export function statusCodeTone(code: number | null): Tone {
  if (code === null) return 'neutral';
  return code >= 200 && code < 300 ? 'ok' : 'bad';
}

export const DELIVERY_STATES: { value: DeliveryState; label: string; tone: Tone }[] = [
  { value: 'pending', label: 'Pending', tone: 'warn' },
  { value: 'delivering', label: 'Delivering', tone: 'info' },
  { value: 'succeeded', label: 'Succeeded', tone: 'ok' },
  { value: 'failed', label: 'Failed', tone: 'bad' },
];

export const deliveryStateMeta = (s: DeliveryState) => DELIVERY_STATES.find((x) => x.value === s) ?? { value: s, label: s, tone: 'neutral' as Tone };

/** One line about how a delivery went: "HTTP 200 in 84 ms", "HTTP 500 · connection reset", "No answer yet". */
export function deliveryOutcome(d: WebhookDelivery): string {
  const parts: string[] = [];
  if (d.last_status_code !== null) parts.push(`HTTP ${d.last_status_code}${d.last_latency_ms !== null ? ` in ${d.last_latency_ms} ms` : ''}`);
  if (d.last_error) parts.push(d.last_error);
  if (parts.length === 0) return d.state === 'pending' || d.state === 'delivering' ? 'No answer yet' : 'No answer';
  return parts.join(' · ');
}

const FIELD_NOUN = [
  ['name', 'The name'],
  ['url', 'The URL'],
  ['format', 'The format'],
  ['events', 'The events'],
  ['repositories', 'The repositories'],
  ['secret', 'The secret'],
] as const satisfies readonly (readonly [keyof WebhookErrors, string])[];

/**
 * The server's field errors (details.fields) in sentences: "is refused by the
 * outbound address policy …" under URL reads "The URL is refused by …".
 */
export function webhookFieldErrors(err: unknown): WebhookErrors {
  const raw = fieldErrorsOf(err);
  const out: WebhookErrors = {};
  for (const [key, noun] of FIELD_NOUN) {
    const msg = raw[key];
    if (msg) out[key] = cleanServerMessage(/^(is|must|are)\b/.test(msg) ? `${noun} ${msg}` : msg);
  }
  return out;
}
