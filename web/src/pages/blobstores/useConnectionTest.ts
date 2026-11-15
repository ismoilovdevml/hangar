import { useState } from 'react';
import { useTestBlobStore } from '../../api/catalog';
import type { BlobStoreInput, ConnectionTestResult } from '../../api/types';

const keyOf = (input: BlobStoreInput) => JSON.stringify(input);

/**
 * "Test before saving" for S3 settings: the first save runs the connection
 * test and saves only when it passes; after a failed test the next save goes
 * through anyway (the button says so). A result belongs to the exact
 * settings tested; any edit makes it stale.
 */
export function useConnectionTest() {
  const test = useTestBlobStore();
  const [tested, setTested] = useState<{ key: string; result: ConnectionTestResult } | null>(null);

  const resultFor = (input: BlobStoreInput) => (tested && tested.key === keyOf(input) ? tested.result : null);

  const run = (input: BlobStoreInput, then?: (r: ConnectionTestResult) => void) =>
    test.mutate(input, {
      onSuccess: (result) => {
        setTested({ key: keyOf(input), result });
        then?.(result);
      },
    });

  /** Saves when the settings passed (or failed and the user saves anyway); otherwise tests first. */
  const submit = (input: BlobStoreInput, save: () => void) => {
    if (resultFor(input)) {
      save();
      return;
    }
    run(input, (r) => r.ok && save());
  };

  return {
    run,
    submit,
    resultFor,
    pending: test.isPending,
    error: test.isError ? test.error : null,
    reset: () => {
      test.reset();
      setTested(null);
    },
  };
}
