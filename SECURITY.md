# Security policy

OpenConvo stores potentially sensitive community conversations, so security
reports are taken seriously.

## Reporting a vulnerability

Report vulnerabilities privately through
[GitHub Security Advisories](https://github.com/openconvo/openconvo/security/advisories/new)
by selecting **Report a vulnerability**. Do not open a public issue.

If the form is unavailable, email
[security@openconvo.com](mailto:security@openconvo.com). Do not include
vulnerability details in a public issue or discussion.

Include:

- the affected OpenConvo version or commit;
- deployment details relevant to the issue;
- clear reproduction steps or a proof of concept;
- the impact you believe the issue has; and
- any known mitigations.

Remove passwords, Discord tokens, access keys and unrelated private message
content from diagnostics. Share sensitive archive data only when it is
strictly necessary to reproduce the issue.

We aim to acknowledge a report within three business days. Please allow time
for investigation and a coordinated fix before publishing details.

## Supported versions

During the pre-1.0 period, security fixes target the latest commit on `main`
and, when a tagged release exists, the latest tagged release. Older releases,
commits and development branches are unsupported. This section will be updated
when OpenConvo adopts a longer support policy.

## Scope notes for self-hosters

- Archive pages are administrator-only in the current release. Do not expose
  an installation publicly without a TLS-terminating reverse proxy.
- PostgreSQL is intentionally not published by the default Compose file.
- Discord tokens and storage credentials belong in `.env` (gitignored), never
  in the repository or database.
- Remote MCP search is a separate read path into private archive content. Keep
  it disabled unless it is served over HTTPS with a dedicated bearer token.
