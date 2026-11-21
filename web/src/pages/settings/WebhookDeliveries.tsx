import { useId, useState, type ReactNode } from 'react';
import { CodeBlock } from '../../components/CodeBlock';
import { Dialog } from '../../components/Dialog';
import { ErrorNote } from '../../components/ErrorNote';
import { Pill } from '../../components/Pill';
import { Select } from '../../components/Select';
import { EmptyState, ErrorMessage, Loading, NoMatches } from '../../components/States';
import { Time } from '../../components/Time';
import { errorText } from '../../lib/apiError';
import { DELIVERY_STATES, deliveryOutcome, deliveryStateMeta, eventLabel } from './webhookModel';
import { useDeliveries, useDelivery, useRedeliver, type DeliveryState, type Webhook, type WebhookDelivery } from './webhooksApi';

type StateFilter = DeliveryState | 'all';

const FILTERS: { value: StateFilter; label: string }[] = [{ value: 'all', label: 'Every state' }, ...DELIVERY_STATES.map((s) => ({ value: s.value, label: s.label }))];

/** The deliveries of one webhook, newest first: filter by state, page through, open one to see what was sent and send it again. */
export function WebhookDeliveries({ hook, canAdmin, onClose }: { hook: Webhook; canAdmin: boolean; onClose: () => void }) {
  const filterId = useId();
  const [filter, setFilter] = useState<StateFilter>('all');
  // Cursors of the pages before the current one, so Previous works with opaque cursors.
  const [cursors, setCursors] = useState<string[]>([]);
  const [cursor, setCursor] = useState('');
  const [open, setOpen] = useState<string | null>(null);
  const state = filter === 'all' ? undefined : filter;
  const page = useDeliveries(hook.id, state, cursor);
  const items = page.data?.items ?? [];
  const next = page.data?.next_cursor ?? null;

  const changeFilter = (f: StateFilter) => {
    setFilter(f);
    setCursor('');
    setCursors([]);
    setOpen(null);
  };

  let body: ReactNode;
  if (page.isError) {
    body = <ErrorNote what="the deliveries" error={page.error} privilege="system:webhooks:read" onRetry={() => void page.refetch()} />;
  } else if (page.isPending) {
    body = <Loading label="Loading deliveries…" />;
  } else if (items.length === 0) {
    body = state ? (
      <NoMatches what="deliveries" onClear={() => changeFilter('all')} />
    ) : (
      <EmptyState title="No deliveries yet">Events this webhook subscribes to show up here once they happen. Finished deliveries are kept for 30 days.</EmptyState>
    );
  } else {
    body = (
      <ul className="deliveries" aria-label="Deliveries" aria-busy={page.isFetching || undefined}>
        {items.map((d) => (
          <DeliveryRow key={d.id} hook={hook} d={d} open={open === d.id} onToggle={() => setOpen(open === d.id ? null : d.id)} canAdmin={canAdmin} />
        ))}
      </ul>
    );
  }

  return (
    <Dialog variant="drawer" title={`Deliveries of ${hook.name}`} description={hook.url} onClose={onClose} className="deliveries-drawer">
      <div className="deliveries-view">
        <div className="deliveries-view__filter">
          <label htmlFor={filterId} className="field__label">
            State
          </label>
          <Select id={filterId} value={filter} options={FILTERS} onChange={changeFilter} size="sm" />
        </div>
        {body}
        {(cursors.length > 0 || next) && (
          <nav className="deliveries-view__pager" aria-label="Pages of deliveries">
            <button
              type="button"
              className="button button--secondary button--sm"
              disabled={cursors.length === 0}
              onClick={() => {
                setCursor(cursors[cursors.length - 1] ?? '');
                setCursors(cursors.slice(0, -1));
                setOpen(null);
              }}
            >
              Previous
            </button>
            <button
              type="button"
              className="button button--secondary button--sm"
              disabled={!next}
              onClick={() => {
                if (!next) return;
                setCursors([...cursors, cursor]);
                setCursor(next);
                setOpen(null);
              }}
            >
              Next
            </button>
          </nav>
        )}
      </div>
    </Dialog>
  );
}

function DeliveryRow({ hook, d, open, onToggle, canAdmin }: { hook: Webhook; d: WebhookDelivery; open: boolean; onToggle: () => void; canAdmin: boolean }) {
  const detailsId = useId();
  const meta = deliveryStateMeta(d.state);
  return (
    <li className={open ? 'delivery is-open' : 'delivery'}>
      <button type="button" className="delivery__row" aria-expanded={open} aria-controls={detailsId} onClick={onToggle}>
        <Pill tone={meta.tone}>{meta.label}</Pill>
        <span className="delivery__what">
          <strong>{eventLabel(d.event)}</strong>
          {(d.repository || d.subject) && <span className="meta"> · {[d.repository, d.subject].filter(Boolean).join(' / ')}</span>}
          {d.redelivery_of && <span className="meta"> · redelivery</span>}
        </span>
        <span className="delivery__outcome meta">{deliveryOutcome(d)}</span>
        <Time value={d.created_at} mode="short" className="delivery__time meta" />
      </button>
      {open && (
        <div id={detailsId} className="delivery__details">
          <DeliveryDetails hook={hook} deliveryId={d.id} canAdmin={canAdmin} />
        </div>
      )}
    </li>
  );
}

function DeliveryDetails({ hook, deliveryId, canAdmin }: { hook: Webhook; deliveryId: string; canAdmin: boolean }) {
  const q = useDelivery(hook.id, deliveryId);
  const redeliver = useRedeliver();
  if (q.isError) return <ErrorNote what="the delivery" error={q.error} onRetry={() => void q.refetch()} />;
  if (q.isPending) return <Loading label="Loading the delivery…" />;
  const d = q.data;
  const payload = d.payload ? JSON.stringify(d.payload, null, 2) : '';
  return (
    <>
      <dl className="facts facts--compact">
        <div>
          <dt>Delivery id</dt>
          <dd>
            <code>{d.id}</code>
          </dd>
        </div>
        <div>
          <dt>Event id</dt>
          <dd>
            <code>{d.event_id}</code>
          </dd>
        </div>
        <div>
          <dt>Attempts</dt>
          <dd>
            {d.attempts} of {d.max_attempts}
          </dd>
        </div>
        <div>
          <dt>Last answer</dt>
          <dd>{deliveryOutcome(d)}</dd>
        </div>
        <div>
          <dt>Created</dt>
          <dd>
            <Time value={d.created_at} seconds />
          </dd>
        </div>
        <div>
          <dt>{d.completed_at ? 'Finished' : 'Next attempt'}</dt>
          <dd>{d.completed_at ? <Time value={d.completed_at} seconds /> : d.next_attempt_at ? <Time value={d.next_attempt_at} mode="future" /> : '—'}</dd>
        </div>
        {d.redelivery_of && (
          <div>
            <dt>Redelivery of</dt>
            <dd>
              <code>{d.redelivery_of}</code>
            </dd>
          </div>
        )}
      </dl>
      {payload ? <CodeBlock lines={payload.split('\n').map((text) => ({ text }))} label={`payload of delivery ${d.id}`} /> : <p className="meta">No payload recorded.</p>}
      {canAdmin && (
        <div className="delivery__actions">
          <button
            type="button"
            className="button button--secondary button--sm"
            disabled={redeliver.isPending || !hook.enabled}
            onClick={() => redeliver.mutate({ id: hook.id, deliveryId: d.id })}
          >
            {redeliver.isPending ? 'Queuing…' : 'Redeliver'}
          </button>
          {!hook.enabled && <span className="meta">Enable the webhook to send events again.</span>}
          {redeliver.isSuccess && (
            <span className="form-success" role="status">
              Queued as a new delivery.
            </span>
          )}
          {redeliver.isError && <ErrorMessage error={redeliver.error} message={errorText(redeliver.error, { action: 'redeliver it' })} />}
        </div>
      )}
    </>
  );
}
