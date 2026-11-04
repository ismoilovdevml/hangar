# Security policy

## Reporting a vulnerability

Please report vulnerabilities privately, not in public issues or pull
requests: use GitHub's **Report a vulnerability** button on the repository's
Security tab (private vulnerability reporting).

Include what you can of: the affected version (`hangar version`), the
component (API, a package format, the web UI, configuration), steps or a
proof of concept, and the impact you see.

What to expect:

- an acknowledgement within 3 working days;
- an assessment and a planned fix date within 10 working days;
- a fix released as a patch version, with a GitHub security advisory that
  credits you unless you prefer otherwise.

Please give us a reasonable time to release a fix before disclosing the
issue publicly, and do not access, change or delete data that is not yours
while testing.

## Supported versions

Security fixes go to the latest release. Hangar has not reached 1.0; until
then, upgrade to the newest version to receive fixes.

## Hardening

How Hangar stores secrets and tokens, the threat model and the operator
checklist are in [docs/security.md](docs/security.md).
