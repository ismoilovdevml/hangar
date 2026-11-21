## What and why

<!-- What this changes and the problem it solves. Link the issue: "Fixes #123". -->

## How it was tested

<!-- Commands you ran and their result; for format changes, the real client
     you tried (e.g. "npm 11 publish and install against a hosted repo"). -->

## Checklist

- [ ] `make test` with `HANGAR_TEST_DATABASE_URL` set (not `HANGAR_TEST_SKIP_DB=1`)
- [ ] `make lint`
- [ ] `make deploy-check`, if `deploy/`, the Helm chart or the alert rules changed
- [ ] Tests cover the change (a failing test first for a bug fix)
- [ ] `api/openapi.yaml` updated for API changes
- [ ] Docs updated (guides, `docs/deploy`, `docs/operations`, `hangar.example.yaml`)
- [ ] A line under `## Unreleased` in `docs/changelog.md` if users or operators would notice
- [ ] No secrets, tokens or internal host names in code, tests, fixtures or logs

<!-- Security vulnerabilities: do not open a pull request. Report them
     privately as described in SECURITY.md. -->
