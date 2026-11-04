import { useState } from 'react';
import { Link } from '@tanstack/react-router';
import { useSession } from '../api/hooks';
import { clientSetup, installCommands, type ClientTarget, type PackageVersion } from '../lib/clientSnippets';
import { FORMAT_META } from '../lib/format';
import { CodeBlock } from './CodeBlock';
import { Segmented } from './Segmented';

interface ClientSetupProps {
  /** One or more repositories; with several, a switch picks which one to show. */
  targets: Omit<ClientTarget, 'username'>[];
  /** A package version: commands that install exactly it come first, one tab per client. */
  install?: PackageVersion;
}

/**
 * How to point a package manager at a repository: what the snippet is for, the
 * set-up commands with the signed-in username filled in, the publish commands
 * when the repository accepts uploads, and where to get a token.
 */
export function ClientSetup({ targets, install }: ClientSetupProps) {
  const username = useSession().data?.user.username;
  const [chosen, setChosen] = useState(0);
  const [client, setClient] = useState('');
  const index = chosen < targets.length ? chosen : 0;
  const target = targets[index];
  if (!target) return null;
  const { setup, publish } = clientSetup({ ...target, username });
  const name = FORMAT_META[target.format]?.label ?? target.format;
  const commands = install ? installCommands(target, install) : [];
  const command = commands.find((x) => x.id === client) ?? commands[0];

  return (
    <div className="client-setup">
      <p className="client-setup__intro">
        Configure your package manager to use Hangar. Use a personal access token where the commands say{' '}
        <code className="inline-code">&lt;token&gt;</code>.{' '}
        <Link to="/account" hash="my-tokens-title" className="link-strong">
          Create an access token
        </Link>
      </p>
      {targets.length > 1 && (
        <Segmented
          label="Client"
          className="client-setup__switch"
          value={String(index)}
          options={targets.map((t, i) => ({ value: String(i), label: FORMAT_META[t.format]?.label ?? t.format }))}
          onChange={(v) => setChosen(Number(v))}
        />
      )}
      {install && command && (
        <div className="client-setup__block">
          <p className="client-setup__label">
            Install {install.name} {install.version}
          </p>
          {commands.length > 1 && (
            <Segmented
              label="Install with"
              className="client-setup__switch"
              value={command.id}
              options={commands.map((x) => ({ value: x.id, label: x.label }))}
              onChange={setClient}
            />
          )}
          <CodeBlock label={`${command.label} install command`} lines={command.lines} />
        </div>
      )}
      <div className="client-setup__block">
        <p className="client-setup__label">
          Use <span className="client-setup__repo">{target.repository}</span>
        </p>
        <CodeBlock label={`${name} set-up commands`} lines={setup} />
      </div>
      {publish.length > 0 && (
        <div className="client-setup__block">
          <p className="client-setup__label">Publish</p>
          <CodeBlock label={`${name} publish commands`} lines={publish} />
        </div>
      )}
    </div>
  );
}
