/** A dash for a value the server does not report, with the reason spelled out for screen readers. */
export function NotAvailable({ reason = 'not available' }: { reason?: string }) {
  return (
    <>
      <span aria-hidden="true">—</span>
      <span className="visually-hidden">{reason}</span>
    </>
  );
}
