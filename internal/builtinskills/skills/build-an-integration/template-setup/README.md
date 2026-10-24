# Deals: a bundle whose own setup flow makes its connection

A complete example of an integration with `"method": "setup"`, for an
imaginary service, Deals, whose app is created by API from a one-time
setup key. Copy it to start a bundle whose connection takes more than a
pasted token or a standard sign-in; for those, start from `template/`.

## What the owner sees

1. A Form asking for a setup key, which setup uses once and keeps
   nowhere.
2. A progress bar while setup trades the key for a session, creates
   the app with the scopes agents need, installs it and reads its
   token.
3. Done: the app's token and id are kept, and the integration is
   installed.

Where Deals does not hand the token back, step 2 ends instead with a
Wait naming the app's page in Deals, checked every few seconds until the
owner has shown the app's token there, then a Form for the token, then
Done.

## The parts

- `manifest.json`
  - `kivali.connection.method` is `setup`, and `kivali.connection.setup`
    declares everything the flow may do, which the install screen lists
    before anything runs and Kivali holds every step to:
    - `summary`: what setup does, in words for the owner.
    - `hosts`: where a Go step may send the browser and a Wait may
      point (`app.deals.example`). The API the server calls is not
      here: it is in `kivali.hosts`, the hosts the pod's proxy lets the
      server reach.
    - `fields`: what a Form may ask for. `setup_key` is `secret` and
      `setup_only`: the owner's entry reaches the server in the next
      call's answer and is never stored. `token` is `secret`: the
      fallback's Form asks for it, and the step keeps it.
    - `keeps`: the keys a step may store in the connection's Secret,
      with words for the owner: `token`, a secret, and `app_id`, which
      Deals shows in its own addresses and so is declared
      `"secret": false`.
  - `user_config` is empty: the connection's values are what setup
    keeps. `mcp_config.env` names them as `${user_config.token}` and
    `${user_config.app_id}`, so the server reads them from its
    environment.
- `server/main.py`, a stdio MCP server, standard library only:
  - `kivali/setup` (`kivali_setup`, `setup`): one call of the flow.
    Kivali calls it first with an empty answer, then after each step
    with what the step gave back (`answer.form`, `answer.return`,
    `answer.checked`), until a step is done or the call answers an
    error. Each answer names its next step: `{"step": {"form": ...}}`,
    `{"go": ...}`, `{"wait": ...}` or `{"done": true}`, with `keep` for
    what to store now and `state` for what the next call needs. Where
    the flow is lives in `state` (`at`), never in the server, because
    each Keep restarts the server with what was kept.
  - `build`: the long step. It runs inside one call, under Kivali's
    five minutes, and reports each stage as an MCP
    `notifications/progress` naming the request's `progressToken`, with
    `progress` and `total` for the bar. A setup key Deals refuses gives
    the Form back with words saying why, rather than failing the flow.
    It takes up the app an earlier run made (found by `APP_NAME`)
    before making one: Kivali calls nothing when a setup is cancelled
    or fails, so that app is still in Deals.
  - The fallback (`show_token_wait`): a Wait whose check (`answer.checked`)
    asks Deals, with a four-second timeout, whether the token has been
    shown, and answers the same Wait until it has; a check that cannot
    reach Deals answers the same Wait too, since an error would end the
    setup. The short-lived session rides in `state`, which
    Kivali keeps in its memory alone and replaces in every word it
    shows with `[secret]`. Kivali does the same to every secret kept
    value, wherever it appears in a step. The app's id is kept and not
    secret, so the Wait's page is the app's own, its id in the address,
    and the check reads the id back from the connection rather than
    from `state`.
  - `kivali/requirements` (`kivali_requirements`): the integration's
    health after Kivali's own rows, `{"requirements": [{"id", "title",
    "words", "met", "fix"}]}`. It answers with no connection too, and
    within Kivali's five seconds (its check of the token waits four).
    `fix` names the flow the row's Fix button runs: `setup` installs
    again; `rescope` is a second flow of `kivali/setup`, a Form for a
    new key and a rebuild with this version's `SCOPES`.
  - `kivali/uninstall` (`kivali_uninstall`): the app removed in Deals,
    answered `{}`. An app already gone, or a token Deals no longer
    takes, is done too, so an uninstall Kivali asks for again finishes.
    An error is a JSON-RPC error whose message the owner reads, and the
    uninstall is refused with nothing changed.
  - An error of any of the three is a JSON-RPC error whose message is
    the owner's words (`Refusal`, `Status`).
  - `tools/list` and `tools/call` as in `template/`, the agents'
    actions reading the kept token from the environment.
  - The output carries JSON-RPC alone, and every request that waits on
    the service runs on a thread, so Kivali's ping is answered while a
    step builds the app.

## Trying it

`scripts/try.py` drives the flow as Kivali does, against
`scripts/deals_stub.py`, a stub of Deals on this machine (`--stub`
starts it and stops it at the end, showing what it was asked;
`DEALS_API` points the server at it). From the skill's directory:

```
python3 scripts/try.py --stub 'python3 scripts/deals_stub.py 8765' --env DEALS_API=http://127.0.0.1:8765 \
  template-setup setup --answer setup_key=sk-test-1
python3 scripts/try.py --stub 'python3 scripts/deals_stub.py 8765 --token-on-page' --env DEALS_API=http://127.0.0.1:8765 \
  --every 0 --checks 1 template-setup setup --answer setup_key=sk-test-1 \
  setup --answer setup_key=sk-test-2 --answer token=app-token-0001
python3 scripts/try.py --stub "python3 scripts/deals_stub.py 8765 --installed --scopes ''" --env DEALS_API=http://127.0.0.1:8765 \
  --field token=app-token-0001 --field app_id=app_1 \
  template-setup requirements setup rescope --answer setup_key=sk-test-3 requirements uninstall uninstall
```

The first installs. The second is the fallback, left at the Wait after
one check and started again: the first run's connection is forgotten,
and the second takes up the app it made. The third is an installed
integration whose app lacks this version's scope: the requirements,
the Fix, the requirements again, and an uninstall asked for twice.

`setup` prints each step as the owner's page shows it, each progress
notification, and what each step keeps; a Form takes its fields from
`--answer` (in order, a key given twice answering two Forms), a Go's
return from `--return`, and a Wait is checked until it changes.
`--field` starts the server with a connection, by the keys setup keeps.
Each step is checked against the manifest's declarations as Kivali
checks it; a `problem:` line is a step Kivali would fail the flow on.
