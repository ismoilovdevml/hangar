import { PageHeader } from '../../components/PageHeader';
import { Panel } from '../../components/Panel';

/** Shown instead of a Settings page to users without system:settings:read. */
export function NoPermission({ what }: { what: string }) {
  return (
    <div className="screen settings-screen">
      <PageHeader eyebrow="Settings" title="Administrators only" />
      <Panel>
        <p className="meta">
          You need the <code>system:settings:read</code> privilege to see the {what}. Ask an administrator if you need access.
        </p>
      </Panel>
    </div>
  );
}
