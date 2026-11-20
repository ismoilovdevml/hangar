import { describe, expect, it } from 'vitest';
import { ApiRequestError } from '../api/client';
import { errorText } from './apiError';

const api = (status: number, code?: string, message?: string) =>
  new ApiRequestError(status, code === undefined ? null : { code, message: message ?? '' });

describe('errorText', () => {
  it('names the refused action on a 403', () => {
    expect(errorText(api(403, 'forbidden', 'forbidden'), { action: 'delete the blob store' })).toBe('You do not have permission to delete the blob store.');
    expect(errorText(api(403, 'forbidden', 'forbidden'))).toBe('You do not have permission to do this.');
  });

  it('keeps the special 403s apart from a missing privilege', () => {
    expect(errorText(api(403, 'read_only_token', 'read-only'), { action: 'save' })).toMatch(/read-only token/);
    expect(errorText(api(403, 'csrf_failed', 'csrf'), { action: 'save' })).toMatch(/Reload it/);
    expect(errorText(api(403, 'password_change_required', 'x'))).toMatch(/^Set a new password first/);
  });

  it('prefers the caller’s sentence for a status', () => {
    const opts = { action: 'add the blob store', status: { 409: 'A blob store with this name already exists.' } };
    expect(errorText(api(409, 'already_exists', 'blob store "x": already exists'), opts)).toBe('A blob store with this name already exists.');
  });

  it('leads with the server’s cleaned message where it is specific', () => {
    expect(errorText(api(400, 'invalid', 'bucket: must not be empty: invalid argument'), { action: 'save' })).toBe('Bucket: must not be empty.');
    expect(errorText(api(503, 'unavailable', 'database is unavailable'))).toBe('Database is unavailable.');
  });

  it('says what could not be done when the answer is not the API’s', () => {
    expect(errorText(api(404), { action: 'scan this package' })).toBe('The server could not scan this package.');
    expect(errorText(api(502), { action: 'scan this package' })).toMatch(/could not reach a service/);
  });

  it('tells a lost connection from other failures', () => {
    expect(errorText(new TypeError('Failed to fetch'), { action: 'save' })).toBe('Cannot reach the server. Check your connection and try again.');
    expect(errorText(new Error('boom'), { action: 'read the file' })).toBe('Could not read the file. Try again.');
    expect(errorText('weird')).toBe('Something went wrong. Try again.');
  });
});
