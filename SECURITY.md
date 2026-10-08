# Security policy

Kivali runs AI agents that execute shell commands, reach the internet
through an allowlist and read your files, so we take security reports
seriously.

## Reporting a vulnerability

Please report vulnerabilities privately through GitHub:
**[Report a vulnerability](https://github.com/kivali-ai/kivali/security/advisories/new)**
(the Security tab of this repository, then "Report a vulnerability").

Do not open a public issue, discussion or pull request for a security
problem. If the "Report a vulnerability" button is unavailable, open an
issue asking a maintainer for a private channel, without any details of
the problem.

Include what you can:

- the affected component (server, agent pods, egress proxy, desktop app,
  supervisor, VM image, Helm chart),
- the version or commit,
- steps to reproduce, and what an attacker gains.

We will acknowledge your report, keep you updated while we work on a fix,
and credit you in the advisory unless you prefer otherwise.

## Supported versions

Security fixes land in the latest release. Kivali Desktop updates itself,
and self-hosted installs should upgrade to the latest chart.

## Scope

In scope: anything that lets someone sign in who should not, read or
change a team's data without permission, escape an agent's sandbox or its
egress allowlist, get an agent to act past an approval it needs, or
tamper with updates and release artifacts.

How Kivali isolates agents and what leaves your machine is described in
[docs/security-and-privacy.md](docs/security-and-privacy.md).
