import { describe, expect, it } from 'vitest';
import { ApiRequestError } from '../../api/client';
import { endpointOf, NEW_WEBHOOK, secretMustBeRenewed, urlError, validateWebhook, webhookFieldErrors, webhookForm, webhookInput, webhookStatus } from './webhookModel';
import type { Webhook } from './webhooksApi';

const stored: Webhook = {
  id: 'w1',
  name: 'ci',
  url: 'https://ci.example.com/***',
  events: ['component.published'],
  repositories: [],
  format: 'generic',
  secret_configured: true,
  enabled: true,
  consecutive_failures: 0,
  disabled_reason: '',
  last_delivery_at: null,
  last_status_code: null,
  created_at: '2026-11-01T00:00:00Z',
  updated_at: '2026-11-01T00:00:00Z',
  updated_by: 'admin',
};

describe('webhook form', () => {
  it('checks the URL like the server', () => {
    expect(urlError('https://hooks.slack.com/services/x')).toBeNull();
    expect(urlError('')).toMatch(/Enter the address/);
    expect(urlError('ftp://x.example')).toMatch(/http/);
    expect(urlError('https://user:pw@x.example/')).toMatch(/user name and password/);
    expect(urlError('https://x.example/#a')).toMatch(/fragment/);
    expect(urlError('https://x.example/***')).toMatch(/shortened URL/);
  });

  it('checks names and repository patterns with the server’s alphabet', () => {
    const ok = { ...NEW_WEBHOOK, name: 'Deploys to chat', url: 'https://x.example/h' };
    expect(validateWebhook(ok, null)).toEqual({});
    expect(validateWebhook({ ...ok, name: '-bad' }, null).name).toBeDefined();
    expect(validateWebhook({ ...ok, events: [] }, null).events).toBeDefined();
    expect(validateWebhook({ ...ok, repositories: ['npm-*', 'maven-releases'] }, null)).toEqual({});
    for (const bad of ['npm.internal', 'NPM', '[a-']) expect(validateWebhook({ ...ok, repositories: [bad] }, null).repositories).toBeDefined();
  });

  it('keeps the stored URL and secret unless they are replaced', () => {
    const f = webhookForm(stored);
    expect(validateWebhook(f, stored)).toEqual({});
    const body = webhookInput(f, stored);
    expect(body).not.toHaveProperty('url');
    expect(body).not.toHaveProperty('secret');
    expect(body).not.toHaveProperty('clear_secret');
    expect(webhookInput({ ...f, secretChoice: 'clear' }, stored)).toMatchObject({ clear_secret: true });
    expect(validateWebhook({ ...f, secretChoice: 'replace' }, stored).secret).toMatch(/Enter the new secret/);
    expect(webhookInput({ ...f, secretChoice: 'replace', secret: 's' }, stored)).toMatchObject({ secret: 's' });
    expect(webhookInput({ ...f, replaceUrl: true, url: ' https://y.example/h ' }, stored)).toMatchObject({ url: 'https://y.example/h' });
  });

  it('knows when a new URL needs the secret again', () => {
    expect(endpointOf('https://ci.example.com/***')).toBe('https://ci.example.com:443');
    expect(endpointOf('http://ci.example.com:8080/x')).toBe('http://ci.example.com:8080');
    const f = { ...webhookForm(stored), replaceUrl: true };
    expect(secretMustBeRenewed({ ...f, url: 'https://ci.example.com:443/other' }, stored)).toBe(false);
    expect(secretMustBeRenewed({ ...f, url: 'https://ci2.example.com/h' }, stored)).toBe(true);
    expect(secretMustBeRenewed({ ...f, url: 'http://ci.example.com/h' }, stored)).toBe(true);
    expect(secretMustBeRenewed({ ...f, url: 'https://ci2.example.com/h', secretChoice: 'clear' }, stored)).toBe(false);
    expect(secretMustBeRenewed({ ...f, url: 'https://ci2.example.com/h' }, { ...stored, secret_configured: false })).toBe(false);
  });

  it('words the server’s field errors as sentences', () => {
    const err = new ApiRequestError(400, {
      code: 'invalid_argument',
      message: 'invalid webhook: …',
      details: { fields: { url: 'is refused by the outbound address policy', name: 'use 1 to 64 letters', other: 'x' } },
    });
    expect(webhookFieldErrors(err)).toEqual({ url: 'The URL is refused by the outbound address policy.', name: 'Use 1 to 64 letters.' });
    expect(webhookFieldErrors(new ApiRequestError(500, { code: 'internal', message: 'x' }))).toEqual({});
  });

  it('tells a webhook the server disabled from one disabled by hand', () => {
    expect(webhookStatus(stored)).toEqual({ label: 'Enabled', tone: 'ok' });
    expect(webhookStatus({ ...stored, consecutive_failures: 3 }).label).toBe('Failing');
    expect(webhookStatus({ ...stored, enabled: false }).label).toBe('Disabled');
    expect(webhookStatus({ ...stored, enabled: false, disabled_reason: 'x' }).label).toBe('Disabled by the server');
  });
});
