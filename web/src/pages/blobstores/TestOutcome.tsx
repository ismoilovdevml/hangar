import { ApiRequestError } from '../../api/client';
import type { ConnectionTestResult } from '../../api/types';

const STEP: Record<string, string> = { bucket: 'bucket', write: 'write', read: 'read back', delete: 'delete' };

/** What the last connection test said, in one line. */
export function TestOutcome({ result, pending, error }: { result: ConnectionTestResult | null; pending: boolean; error: unknown }) {
  if (pending) {
    return (
      <p className="field__hint" role="status">
        Testing the connection…
      </p>
    );
  }
  if (error) {
    const msg = error instanceof ApiRequestError ? (error.body?.message ?? 'The server could not test the settings.') : 'Cannot reach the server.';
    return (
      <p className="form-error" role="alert">
        {msg}
      </p>
    );
  }
  if (!result) return null;
  if (result.ok) {
    return (
      <p className="store-test is-ok" role="status">
        Connected: {result.steps.map((s) => STEP[s.name] ?? s.name).join(', ')} work · {result.latency_ms} ms
      </p>
    );
  }
  const failed = result.steps.find((s) => s.status === 'failed');
  return (
    <p className="form-error" role="alert">
      Connection test failed{failed ? ` (${STEP[failed.name] ?? failed.name})` : ''}: {result.message}
    </p>
  );
}
