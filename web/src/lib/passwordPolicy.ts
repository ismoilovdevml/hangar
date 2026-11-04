import type { Session } from '../api/types';

export type PasswordPolicy = NonNullable<Session['password_policy']>;

/** The server's default (`auth.password`), for sessions from older servers. */
export const DEFAULT_PASSWORD_POLICY: PasswordPolicy = { min_length: 12, max_length: 256, min_classes: 0, allow_username: false };

export interface PasswordCheck {
  id: 'length' | 'classes' | 'username' | 'differs' | 'match';
  label: string;
  met: boolean;
}

/** Character classes the server counts: lower case, upper case, digits, anything else. */
export function characterClasses(pw: string): number {
  let lower = false;
  let upper = false;
  let digit = false;
  let other = false;
  for (const ch of pw) {
    if (/\p{Ll}/u.test(ch)) lower = true;
    else if (/\p{Lu}/u.test(ch)) upper = true;
    else if (/\p{Nd}/u.test(ch)) digit = true;
    else other = true;
  }
  return [lower, upper, digit, other].filter(Boolean).length;
}

/**
 * The rules a new password must meet, checked live as the user types. They
 * mirror the server's password policy; the server stays the authority.
 */
export function passwordChecks(policy: PasswordPolicy, username: string, current: string, next: string, confirm: string): PasswordCheck[] {
  const length = [...next].length;
  const checks: PasswordCheck[] = [
    {
      id: 'length',
      label:
        policy.max_length > 0 && length > policy.max_length
          ? `At most ${policy.max_length} characters`
          : `At least ${policy.min_length} characters`,
      met: length >= policy.min_length && (policy.max_length <= 0 || length <= policy.max_length) && next.trim() !== '',
    },
  ];
  if (policy.min_classes > 0) {
    checks.push({
      id: 'classes',
      label: `At least ${policy.min_classes} of: lower case, upper case, digits, symbols`,
      met: characterClasses(next) >= policy.min_classes,
    });
  }
  if (!policy.allow_username && username) {
    checks.push({
      id: 'username',
      label: 'Does not contain your username',
      met: next !== '' && !next.toLowerCase().includes(username.toLowerCase()),
    });
  }
  checks.push(
    { id: 'differs', label: 'Different from the current password', met: next !== '' && next !== current },
    { id: 'match', label: 'Both new password fields match', met: next !== '' && next === confirm },
  );
  return checks;
}
