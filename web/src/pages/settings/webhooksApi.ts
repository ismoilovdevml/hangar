/**
 * Data access for outgoing webhooks (api/openapi.yaml, tag webhooks). Reads
 * need system:webhooks:read, changes system:webhooks:admin; the server also
 * wants read access to every repository a webhook names. Queries use the
 * app-wide retry policy (lib/queryClient); mutations are never retried, so a
 * test ping or a redelivery is sent once per click.
 */
import { keepPreviousData, useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { apiFetch } from '../../api/client';
import type { components, operations } from '../../api/schema';

type Schemas = components['schemas'];

export type Webhook = Schemas['Webhook'];
export type WebhookInput = Schemas['WebhookInput'];
export type WebhookEvent = Schemas['WebhookEvent'];
export type WebhookFormat = Schemas['WebhookFormat'];
export type WebhookDelivery = Schemas['WebhookDelivery'];
export type WebhookDeliveryPage = Schemas['WebhookDeliveryPage'];
export type WebhookPayload = Schemas['WebhookPayload'];
export type DeliveryState = NonNullable<NonNullable<operations['listWebhookDeliveries']['parameters']['query']>['state']>;

/** Deliveries per page in the deliveries view. */
export const DELIVERY_PAGE_SIZE = 25;

export const webhookKeys = {
  all: ['webhooks'] as const,
  list: ['webhooks', 'list'] as const,
  deliveries: (id: string, state: DeliveryState | undefined, cursor: string) => ['webhooks', 'deliveries', id, state ?? 'all', cursor] as const,
  delivery: (id: string, deliveryId: string) => ['webhooks', 'delivery', id, deliveryId] as const,
};

const path = (id: string, rest = '') => `/webhooks/${encodeURIComponent(id)}${rest}`;

export function useWebhooks(enabled = true) {
  return useQuery({
    queryKey: webhookKeys.list,
    queryFn: ({ signal }) => apiFetch<{ items: Webhook[] }>('/webhooks', { signal }),
    enabled,
  });
}

/** Changes refresh the list and every deliveries view (a test or an enable changes both). */
function useWebhookMutation<A, R>(fn: (args: A) => Promise<R>) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: fn,
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: webhookKeys.all });
    },
  });
}

export const useCreateWebhook = () => useWebhookMutation((body: WebhookInput) => apiFetch<Webhook>('/webhooks', { method: 'POST', body }));
export const useUpdateWebhook = () =>
  useWebhookMutation(({ id, body }: { id: string; body: WebhookInput }) => apiFetch<Webhook>(path(id), { method: 'PUT', body }));
export const useDeleteWebhook = () => useWebhookMutation((id: string) => apiFetch<void>(path(id), { method: 'DELETE' }));
/** Sends a ping now and answers with the recorded delivery (also when the receiver failed). */
export const useTestWebhook = () => useWebhookMutation((id: string) => apiFetch<WebhookDelivery>(path(id, ':test'), { method: 'POST' }));
export const useRedeliver = () =>
  useWebhookMutation(({ id, deliveryId }: { id: string; deliveryId: string }) =>
    apiFetch<WebhookDelivery>(path(id, `/deliveries/${encodeURIComponent(deliveryId)}:redeliver`), { method: 'POST' }),
  );

export function useDeliveries(id: string, state: DeliveryState | undefined, cursor: string) {
  return useQuery({
    queryKey: webhookKeys.deliveries(id, state, cursor),
    queryFn: ({ signal }) =>
      apiFetch<WebhookDeliveryPage>(path(id, '/deliveries'), { query: { state, cursor: cursor || undefined, limit: DELIVERY_PAGE_SIZE }, signal }),
    placeholderData: keepPreviousData,
  });
}

/** One delivery with the payload that was sent. */
export function useDelivery(id: string, deliveryId: string | null) {
  return useQuery({
    queryKey: webhookKeys.delivery(id, deliveryId ?? ''),
    queryFn: ({ signal }) => apiFetch<WebhookDelivery>(path(id, `/deliveries/${encodeURIComponent(deliveryId!)}`), { signal }),
    enabled: !!deliveryId,
  });
}
