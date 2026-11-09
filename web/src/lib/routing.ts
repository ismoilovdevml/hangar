/** Routing rules editing helpers for the repository settings panel. */
import type { MemberRouting, Repository, RoutingBlock, RoutingRules } from '../api/types';

/** An example pattern per format, shown as the placeholder. */
export const EXAMPLES: Record<string, string> = {
  nuget: 'Edcom.*',
  npm: '@trk/*',
  maven: 'com.edcom',
  pypi: 'edcom-*',
  go: 'git.example.com/corp',
  cargo: 'edcom-*',
  docker: 'team/*',
  helm: 'edcom-*',
  apt: 'edcom-*',
  rpm: 'edcom-*',
  raw: 'internal/*',
};

/** Patterns typed as a comma or space separated list. */
export const parsePatterns = (s: string) => [...new Set(s.split(/[\s,]+/).filter(Boolean))];
export const showPatterns = (ps?: readonly string[]) => (ps ?? []).join(', ');

export interface Draft {
  allow: string;
  block: string;
  members: Record<string, { exclusive: string; block: string }>;
}

export function toDraft(repo: Repository): Draft {
  const rt = repo.config.routing;
  const members: Draft['members'] = {};
  for (const m of repo.config.group?.members ?? []) {
    const r = rt?.members?.find((x) => x.member === m);
    members[m] = { exclusive: showPatterns(r?.exclusive), block: showPatterns(r?.block) };
  }
  return { allow: showPatterns(rt?.allow), block: showPatterns(rt?.block), members };
}

/** The PUT body for a draft. */
export function toRules(repo: Repository, d: Draft): RoutingRules {
  if (repo.type === 'proxy') return { allow: parsePatterns(d.allow), block: parsePatterns(d.block) };
  const members: MemberRouting[] = [];
  for (const m of repo.config.group?.members ?? []) {
    const exclusive = parsePatterns(d.members[m]?.exclusive ?? '');
    const block = parsePatterns(d.members[m]?.block ?? '');
    if (exclusive.length || block.length) members.push({ member: m, exclusive, block });
  }
  return { members };
}

/** One line for a rule that excludes a member or a proxy name. */
export function blockReason(b: RoutingBlock): string {
  if (b.rule === 'allow') return `not in the allow list of ${b.repository}`;
  if (b.rule === 'exclusive') return `${b.pattern} is reserved for ${(b.owners ?? []).join(', ')} in ${b.repository}`;
  return b.member ? `${b.repository} blocks ${b.pattern} from this member` : `${b.repository} blocks ${b.pattern}`;
}

