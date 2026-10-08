# Contributing to Kivali

Thanks for helping. Kivali is built in the open, and bug reports, docs
fixes and code are all welcome.

## Before you start

- **Bugs.** Open an issue with what you did, what you expected and what
  happened. Include where you run Kivali (Kivali Desktop on macOS, or
  your own Kubernetes), the Kivali version, and any error text.
- **Features and larger changes.** Open an issue to talk it through before
  writing code, so we can agree on the shape first.
- **Security problems.** Do not open a public issue. Follow
  [SECURITY.md](SECURITY.md).

## Setting up

The developer guide covers prerequisites, running a team locally, and
the test suites: [docs/developers/README.md](docs/developers/README.md).
The short version:

```
make run     # a demo team at http://127.0.0.1:8080, no sign-in, scripted model (needs Go and Node)
make test    # the quick loop: Go tests, lint, the web app's lint and tests
make ci      # everything CI runs (needs helm, shellcheck, cargo and jq)
```

## Pull requests

- Keep each pull request to one change, with a description of what it does
  and why.
- Add or update tests. Tests are deterministic: no sleeps, no polling.
- Update the docs your change affects, in the same pull request.
- Run `make ci` before you push. CI runs the same targets.
- UI text follows the voice in [design-system/README.md](design-system/README.md);
  `make test` checks it.
- A new or updated dependency must pass `make licenses`.

[AGENTS.md](AGENTS.md) lists the conventions the codebase follows. It is
written for coding agents, and it is just as useful for people.

## License

Kivali is licensed under the [Apache License 2.0](LICENSE). By
contributing, you agree that your contributions are licensed under the
same terms.
