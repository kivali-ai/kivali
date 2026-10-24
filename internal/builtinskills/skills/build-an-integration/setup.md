# A setup connection

This is the build-an-integration skill's contract for a bundle whose
`kivali.connection.method` is `setup`. `SKILL.md`, beside it, holds
everything else a bundle meets: the manifest, the pod, the network, the
protocol, access, `try.py` and publishing.

The bundle's own flow makes the connection: a run of steps the owner
takes, which Kivali draws and runs, with the server deciding each next
step. Kivali calls `kivali/setup` with the owner's last answer, the
server answers with the next step, and so on until a step is Done:

- **Form**: fields for the owner to fill in, in Kivali's page.
- **Go**: the owner's browser goes to a page on the service, by a link
  or by a form posted there, and usually comes back.
- **Wait**: the owner does something on the service, or the service
  does something, while Kivali checks every few seconds.
- **Done**: the connection is made.

Each step may **keep** values in the connection's Secret, which become
the connection the actions use, and carry **state** to the next call.
After the install, `kivali/requirements` is the integration's health:
rows met or not, each with a Fix that runs another flow. On Uninstall,
`kivali/uninstall` undoes the install on the service.

## The declarations

`kivali.connection.setup` declares everything the flow may do. The
install screen lists it under **Setup** before anything runs, and
Kivali holds every step to it: a step outside it fails the flow.

```json
"connection": {
  "method": "setup",
  "setup": {
    "summary": "Builds a private app in your Deals account with the scopes agents need, installs it there, and keeps its token. The setup key you paste is used once and kept nowhere.",
    "hosts": ["app.deals.example"],
    "fields": {
      "setup_key": {
        "title": "Setup key",
        "words": "A one-time setup key, from Settings, Developer, Setup keys, in your Deals account.",
        "secret": true,
        "setup_only": true
      },
      "token": {
        "title": "App token",
        "words": "The app's token, from its page in Deals, under Show token.",
        "secret": true
      }
    },
    "keeps": {
      "token": "The app's token, which every call sends",
      "app_id": { "words": "The app's id in Deals, to rebuild or remove it", "secret": false }
    }
  }
},
"hosts": ["api.deals.example"]
```

| Declaration | What Kivali holds the flow to | The check refuses |
| --- | --- | --- |
| `summary` | The owner reads it on the install screen and the page: what setup does on the service, in one or two sentences. | `kivali.connection.setup.summary is missing; ...` |
| `hosts` | Every host a Go may send the browser to and a Wait may name a page on: `https://` only, no user info, the host exactly as listed (no port, nothing under it). Empty for a flow that stays in Kivali. The API the server calls is not here but in `kivali.hosts`. | a host that is not lowercase, `*`, a pattern, with a scheme, port or path, or listed twice |
| `fields` | Every field a Form may ask for, in order. `words` (required): what to enter and where it comes from. `title`: the label (the key in words otherwise). `secret`: entered as a password, never shown again. `setup_only`: lives in the flow's memory alone, never kept: a key the flow trades for something else. | missing words; a key that is not lowercase letters, digits and underscores, or is one Kivali keeps a sign-in under; `secret` or `setup_only` that is not true or false; a `setup_only` field that is also kept |
| `keeps` | Every key a step may keep, each with words saying what it is: the words alone (`"token": "The app's token"`), a secret, or `{"words": ..., "secret": false}`. Kept values are the connection: all stored in its Secret, given to the server by `${user_config.<key>}` in `mcp_config.env` and in `kivali/credential`'s `fields`. A secret one is shown as `[secret]` wherever the owner, an agent or the log would read it ([State and keep](#state-and-keep)); one that is not is shown as it is. An identifier the service shows publicly (a slug, an app id, an installation id) is `"secret": false`; a credential (a token, a key, a client secret) is the words alone. The install screen marks each secret one. | missing words; a key as for fields; `words` that is not text, `secret` that is not true or false, or any other key in the object |
| `then` | `browser` or `device`: setup ends by handing over to Kivali's sign-in with the app it made ([Sketch: an app made by API, then a sign-in](#sketch-an-app-made-by-api-then-a-sign-in)). Beside `setup`, the connection then gives that sign-in's endpoints (`authorization_endpoint` and `token_endpoint` for browser, `device_endpoint` and `token_endpoint` for device, `revocation_endpoint` optional), `scopes` and `credential_keys.access_token`, as a sign-in connection does. | another value; `then` without `client_id` and `client_secret` in `keeps`; a missing endpoint or `credential_keys.access_token`; endpoints without `then` |
| `user_config` | Empty: the connection's values are what setup keeps. | any field |
| `client` | Not given: the app is the one setup makes. | any value |

The words Kivali shows when a step breaks a declaration, which the
owner reads as why the setup did not finish, and `try.py` as a
`problem:` line:

- "<Integration>'s setup tried to send your browser to <host>, which
  its manifest does not declare"
- "<Integration>'s setup named a page on <host>, which its manifest
  does not declare"
- "<Integration>'s setup asked for <key>, which its manifest does not
  declare"; "<Integration>'s setup asked for a form with no fields"
- "<Integration>'s setup tried to keep <key>, which its manifest does
  not declare"
- "<Integration>'s setup gave no next step"; "<Integration>'s setup
  gave more than one next step"
- "<Integration>'s setup answered with a step Kivali cannot read: ..."
  (a key none of the shapes below has, or a value of another type)
- "<Integration>'s setup named an address longer than 4096
  characters"; "... gave more to keep and carry than Kivali holds for
  a step (64 KiB)"; "... gave a form to post larger than Kivali sends
  (64 KiB)"

## A flow, call by call

Kivali starts a flow when the owner chooses Install (flow `setup`), or
a requirement's Fix (the flow its row names). It calls `kivali/setup`
first with an empty answer, then after each step with what the step
gave back, one call at a time, until a step is Done or a call answers
an error:

```json
{"jsonrpc": "2.0", "id": 7, "method": "kivali/setup", "params": {
  "flow": "setup",
  "answer": {"state": {}, "form": {}, "return": {}, "checked": false, "went": false, "leg": 1,
             "callback": "https://team.example/connect/callback?state=4f0c9e2a7b1d4c3e8a6f5b2d1c0e9f8a",
             "team": "Acme"},
  "_meta": {"progressToken": "8c1d6b0e2f4a"}}}
```

- `flow`: `setup` to install, or a Fix's flow name. A Fix of `setup`
  installs again over the connection there is, with its values in the
  environment.
- `answer.state`: the last step's `state`, as it was; `{}` at first.
- `answer.form`: what the owner entered in the last step's Form, every
  field it asked for, trimmed, one left blank as `""`.
- `answer.return`: the query the service sent the browser back with
  after a Go, each key's values as a list (`{"code": ["x1"], "state":
  ["4f0c..."]}`). Untrusted ([Coming back](#coming-back)).
- `answer.checked`: true for a Wait's check.
- `answer.went`: true after a Go with `no_return` was sent.
- `answer.leg`: which call of the flow this is, from 1.
- `answer.callback`: where a Go's service may send the browser back.
- `answer.team`: the team's name as the owner set it, `""` for a team
  with none: for naming what setup makes on the service ("Kivali Acme"),
  so the owner tells it apart from another team's. `try.py` sends
  `--team` (Kivali by default).
- Every object is there, `{}` when empty.

The answer is one step, and optionally what to keep and to carry:

```json
{"jsonrpc": "2.0", "id": 7, "result": {"step": {
  "form": {"words": "Paste a setup key from your Deals account.", "fields": ["setup_key"]},
  "state": {"at": "key"}}}}
```

| `step` holds | Shape |
| --- | --- |
| exactly one of | `"form": {"words", "fields": [keys]}`; `"go": {"words", "url", "post", "no_return", "stateless"}` (`post` an object of strings, or lists of strings for a key posted more than once; the rest optional); `"wait": {"words", "url", "code"}` (`url` and `code` optional); `"done": true` |
| optionally | `"keep": {key: value}` and `"state": {key: value}`, strings |

An error is a JSON-RPC error, and its message is what the owner reads
under "Setup did not finish", so write it in their words: `{"code":
-32000, "message": "Deals did not create the app: the account has no
developer access. Ask a Deals admin for it, then try again."}`. The
owner then has Try again. A server that answers -32601 (it does not
implement `kivali/setup`) fails every install with "<Integration>'s
server does not answer kivali/setup; its author must add it".

A server not yet running when the flow starts (the install switched it
on) is waited for two minutes at most, after which the flow fails with
"<Integration> is not running, so its setup cannot go on; its log says
why. Start the setup again once it runs".

## Form

For what only the owner knows or can get: a key from the service's
settings, an account or workspace name, a token the service shows once.

- The page shows the step's words, then "<Integration> asks for" and
  each field: its title, its words as the hint (with "Used during
  setup and never kept." for a setup-only one), a password box for a
  secret one. Continue and Cancel.
- Ask only declared fields, and no more than the next call needs.
- Check the answer at once, against the service where you can, and
  answer the same Form again with words saying what was wrong ("Deals
  did not take that setup key; each one works once. Paste a new
  one.") rather than an error: the owner stays on the step.
- What the owner enters, every field, reaches the next call alone, in
  `answer.form`: nothing of it is kept unless a step keeps it. To use
  a value in a later call, carry it in `state`, where Kivali keeps it in
  memory for this flow and never writes it; a setup-only one may never
  be kept.

## Go

For a page on the service the owner acts on: approving an app,
creating one from a manifest, installing it on an account, choosing
what it may reach.

- The page shows the words and "Continue to <host>". Kivali sends the
  browser to `url`: a link, or with `post` a small page that posts the
  values to `url` and submits itself (a Continue button for a browser
  that runs no scripts). Use `post` where the service takes a form
  (a manifest, a large or secret value); never put a secret in a
  link's query.
- With a return (the default), the service sends the browser back to
  `answer.callback`, the owner lands on the integration's page, and
  the next call has `answer.return`.
- With `"no_return": true`, for a settings page that sends the
  browser nowhere: Kivali opens it in a new tab and calls setup again
  at once with `"went": true`. Follow it with a Wait that checks the
  service for what the owner was to do there.
- With `"stateless": true`, for a service that drops the state from
  the address it sends the browser back to (below).
- In Kivali Desktop the system browser goes, signed in to the team, and
  comes back there; nothing differs for the server.
- A team in Kivali Desktop on the owner's computer has a plain `http`
  loopback address, so `answer.callback` is
  `http://127.0.0.1:<port>/connect/callback?...`. A service that takes
  only `https` addresses refuses it: when the callback is `http`, say in
  the Go's words that the service may refuse this address, and that the
  owner then gives the team an `https` address under Other devices in
  Kivali Desktop and starts again from there.

## Coming back

`answer.callback` is `<Kivali's address>/connect/callback?state=<s>`,
with a fresh `<s>` for each call (a Wait's check is given the latest
again). The state is how Kivali finds the flow when the browser comes
back, so it must come back in the query:

- Where the service takes a `state` parameter and passes it through to
  the address it sends the browser back to (OAuth's shape, and GitHub's
  app manifest and installation pages), give the service the bare
  callback, `<Kivali's address>/connect/callback` with no query, as the
  address to come back to, and `<s>` as its `state` parameter, on the
  page's own address where the service reads it there
  (`https://forge.example/settings/apps/new?state=<s>`).
- Where the service sends the browser back to an address you give it
  and passes nothing through, give `answer.callback` whole.
- Kivali takes any state it gave the flow while the flow lives, so an
  address fixed in an earlier call still comes back. The return is
  given to the step on screen: a link replayed from an earlier leg
  arrives as the current Go's `answer.return`.
- Without a state the browser is matched by Kivali's cookie alone,
  which goes with any page the owner opens. So Kivali takes a return
  with no state only for a Go marked `stateless`, and fails any other
  with "the service sent the browser back without its state; start it
  again". Mark `stateless` only where the service may come back without
  the state (it returns to an address of its own settings, such as an
  app's setup address, which you cannot be sure carries it), pass the
  state on as above all the same, and check what comes back all the
  more.
- `answer.return` is what anyone could put in an address: never keep
  it as it came. Trade a code with the service before keeping anything
  (a code that the service refuses is a refusal, not a connection);
  look an id up with the service rather than trusting it; and read a
  return that holds `error` (`error=access_denied`) as the owner
  turning it down, answering with the Go again or a Form and words that
  say so.
- The browser must come back to the browser that left, signed in to
  Kivali, or the flow fails with Kivali's own words; nothing reaches
  the server.

## Wait

For something that happens on the service, at the owner's hand or the
service's own pace: a token shown on a page, an app's approval by an
admin, a build that finishes after the call that started it.

- The page shows the words, a code to copy (`code`, for one the owner
  enters on the service's page) and a link to `url` (on a declared
  host), then "Waiting." and Cancel.
- While the owner's page is open, Kivali calls setup with `"checked":
  true` every three seconds, one call at a time. Nothing is checked
  while nobody watches: a check is a moment the owner is there, not a
  timer. Answer with one quick look at the service (a timeout of a few
  seconds) and the same Wait while nothing has changed; the next step
  once it has.
- A check that cannot reach the service, or gets an answer that may
  pass by itself (a 5xx, a rate limit), answers the same Wait again,
  for the next check to try. An error from a check fails the whole
  flow, so answer one only for what trying again will not mend.
- The same Wait, the same state and nothing kept is no change. A flow
  is forgotten ten minutes after its last change, so a Wait the owner
  does not finish in ten minutes ends the flow: use one for what takes
  minutes, and say in the words what the owner does.

## Done

The connection is made: for an install, the integration is installed
and its requirements are read. A Done may keep what the last call
found. With `then`, Kivali's sign-in takes the flow on with the app
setup kept.

## State and keep

- **`state`** is the flow's memory: where it is (`"at": "key"`), a
  setup-only value, a session or code to trade, what an earlier call
  learned. Each step's `state` replaces the last; a step with none
  carries nothing. It lives in Kivali's memory for this flow alone and
  is never written. The server keeps nothing of the flow itself: it
  may be restarted between calls (each Keep restarts it), and the next
  call may be of a flow started again.
- **`keep`** is the connection: the values the actions use, written to
  the connection's Secret before the step is shown, declared keys
  only. A kept empty value forgets that key. Kivali starts the server
  again with what was kept, so the next call, and every action, reads
  it from the environment (`${user_config.<key>}`) or
  `kivali/credential`'s `fields`. Keep what the actions and the
  requirements need and nothing more.
- An install keeps nothing unless it is Done: when it fails, the owner
  cancels it, or nobody takes its step in ten minutes, Kivali forgets
  the connection it made. A Fix that fails keeps what it kept.
- Every secret kept value, every secret or setup-only value the owner
  entered, every state value, each callback's state, and the values a
  return brings back under `code`, `state` or a key holding `token`,
  `secret`, `password` or `key`, each one six characters or longer, are
  replaced with `[secret]` wherever the owner reads the flow: its words,
  its progress, a Wait's page address, the error, the audit and the
  log. A value kept under a keep declared `"secret": false` is shown as
  it is. So a value the owner must read in a page's address or the
  words (an app's id or name on the service, the account it is
  installed on) is kept under a keep that is not secret, and is not
  also carried in `state`: a later call reads it back from the
  connection.
- A Wait's `code` is shown as given, never scrubbed: it is a code the
  owner enters on the service's page, which the step chose to show.
  Never put a secret there.
- A secret kept value is a secret to agents too: an agent's answer and
  the integration's log show each one, six characters or longer, as
  `[secret]`. An identifier an action gives agents in the clear (the
  app's name as the author of what it writes, an account) is kept
  under a keep declared not secret, or read from the service on the
  call.

## A flow left partway

Kivali calls nothing when the owner cancels a flow, starts another, or
leaves one to expire, and nothing when a call fails. What setup made on
the service before then stays there. So make each step safe to run
again: before creating an app, look for the one an earlier run made
(by the name setup gives it) and take it up; before installing, ask
whether it is installed. The owner who tries again then gets one app,
not two.

## Progress

A call that takes more than a moment says where it is. Send MCP's
progress notification with the request's `_meta.progressToken`, as it
came, while the call runs:

```json
{"jsonrpc": "2.0", "method": "notifications/progress",
 "params": {"progressToken": "8c1d6b0e2f4a", "progress": 2, "total": 4, "message": "Making the app"}}
```

The page shows `message`, and a progress bar of `progress` of `total`
where `total` is above 0: give real counts where there is a count (the
stages of a build, the pages of a list). One naming another token, or
sent after the call answered, is dropped. A call that waits on the
service more than once (build, poll, deploy) is fine inside its five
minutes, with a stage for each wait.

## Requirements

`kivali/requirements` is the installed integration's health. Kivali
asks for it when the owner's page, the list or Home is read, after its
own rows (Approved version, Connection, Sign-in, Connection test,
Running):

```json
{"jsonrpc": "2.0", "id": 9, "method": "kivali/requirements", "params": {}}
{"jsonrpc": "2.0", "id": 9, "result": {"requirements": [
  {"id": "token", "title": "App token", "words": "The app's token works, in Acme.", "met": true},
  {"id": "scopes", "title": "Scopes", "met": false, "fix": "rescope",
   "words": "This version needs deals.write, which the app was not built with. Paste a new setup key to rebuild it."}]}}
```

- Each row has `id`, `title`, `words` and `met`, and optionally `fix`,
  and nothing else; a row lacking one of the four, or holding any other
  key, makes the whole answer unreadable, shown as one unmet row
  "<Integration> could not check its requirements: ...". An id or fix is letters, digits, hyphens and
  underscores, 64 at most, and an id is not one of Kivali's
  (`version`, `connection`, `sign_in`, `connection_test`, `running`,
  `checks`) and given once; a row that breaks this is left out. The
  first 20 rows are kept. A title is cut to 80 characters and words to
  500.
- Answer within five seconds, or the page shows that unmet row: the
  owner's page waits on it. Make one request to the service at most,
  with a timeout of four seconds, and none with no connection: the
  smoke test asks with every value empty and no network. Kivali keeps
  the answer three seconds.
- What to check: the kept token works (and whose it is); what this
  version needs is there (its scopes, permissions, a webhook); the app
  is still installed. Each row a thing the owner can understand on its
  own.
- Words: met says what is so ("The app's token works, in Acme."); not
  met says what is wrong and what the Fix does ("Deals no longer takes
  the app's token. Set it up again to build a new app."). A row that
  cannot be checked now (the service did not answer) is not met, with
  no Fix, and says so.
- `fix` is the flow the row's Fix button runs: `setup` (install again,
  over the connection there is), `sign_in` (for a setup that hands over
  to a sign-in), or a flow of your own, which `kivali/setup` gets as
  `flow`. A Fix of your own runs only while a row names it, and only
  while the integration is installed, on and running ("<Integration>
  is not running, so its own flows cannot run; its log says why"). A
  Fix flow is like an install: steps, progress, keeps, Done. It works
  on the connection there is (its values are in the environment), asks
  only for what the mend needs, and a Fix that fails keeps what it
  kept.
- Unmet rows are in the owner's Needs you until they are met.

## Uninstall

```json
{"jsonrpc": "2.0", "id": 12, "method": "kivali/uninstall", "params": {}}
{"jsonrpc": "2.0", "id": 12, "result": {}}
```

On Uninstall, before Kivali forgets the connection, the server undoes
the install on the service: removes the app, the webhook, the
installation. Answer `{}` once done, within 60 seconds.

- An installation already gone is done: answer `{}`, so an uninstall
  tried again finishes. Kivali may ask again after an answer of `{}`,
  when it could not forget the connection the first time.
- A value it needs that is missing (the token is not in the
  environment): nothing can be undone from here; answer `{}`.
- An error is a JSON-RPC error, and the uninstall is refused with
  nothing changed: the owner reads "<Integration> could not undo its
  installation: <message>; nothing was changed". Refuse only for a
  failure that trying again may get past; for one it never will, the
  owner is stuck until they switch the integration off.
- A switched-off integration is uninstalled without the request, its
  install on the service left as it is. A switched-on one whose server
  is not running cannot be uninstalled ("<Integration> is not running,
  so it cannot undo its installation on the service; switch it off to
  uninstall it without that").

## Words the owner reads

Kivali shows every word setup gives as it is: the summary, each field's
title and words, each keep's words, each step's words, progress
messages, errors, requirement titles and words, Test connection's
answer. Write them as the page's own:

- Sentence case, short sentences, no exclamation marks, no emoji, no
  "please". Speak to the owner as "you". Never "simply", "seamless",
  "bots", "magic" or "AI-powered".
- Say what to do and where, with the service's own names for its
  pages, as the service shows them: "Open Settings, Developer, Setup
  keys in Deals and create a key."
- Say what happens and what is kept: "Kivali uses it once to build the
  app, and keeps it nowhere."
- A failure says what went wrong and what the owner can do: "Deals did
  not take that setup key; each one works once. Paste a new one."
- No words of the bundle's insides: not MCP, tool, pod, server, flow,
  state or step.
- At most 200 characters for a step's words, a progress message and an
  error (Kivali cuts the rest), 64 for a Wait's code, 80 for a
  requirement's title and 500 for its words.

## Bounds and deadlines

| What | Bound |
| --- | --- |
| One `kivali/setup` call | 5 minutes, or the flow fails: "setup did not finish its step within 5 minutes; start it again" |
| A flow | forgotten 10 minutes after its last change; lost when Kivali restarts |
| A Wait's checks | every 3 seconds while the owner's page is open (a read of the flow in the last 10 seconds counts), one at a time |
| The server's start, for a flow | 2 minutes |
| `kivali/requirements` | 5 seconds; 20 rows; the answer kept 3 seconds |
| A requirement's title and words | 80 and 500 characters |
| `kivali/uninstall` | 60 seconds |
| A step's words, progress message, error | 200 characters; a Wait's code 64 |
| A Go's or a Wait's address | 4096 characters |
| `keep` and `state` together, and a Go's `post` | 64 KiB each |

## The example: template-setup

template-setup is a complete setup bundle for Deals, whose app is made
through its API from a setup key the owner pastes. Read its
`server/main.py` beside this.

1. **Install.** The first call has an empty answer; the server answers
   a Form for `setup_key`, with `"state": {"at": "key"}`.
2. **The key.** The owner pastes it; the next call's `answer.form` is
   `{"setup_key": "sk-..."}` and its state `{"at": "key"}`. In this one
   call, `build` trades the key for a session (a refused key answers
   the Form again with words saying why), finds the app an earlier run
   made or creates one with this version's `SCOPES`, installs it and
   reads its token, sending a progress notification at each stage
   (`1 of 4` to `4 of 4`). It answers `{"done": true, "keep": {"token":
   ..., "app_id": ...}}`. Kivali keeps both and starts the server again
   with them in `DEALS_TOKEN` and `DEALS_APP_ID`.
3. **The fallback.** Where Deals does not hand the token back, the call
   keeps `app_id` alone and answers a Wait ("Open the Kivali app in
   Deals and choose Show token.") with the app's own page, its id in
   the address (`app_id` is not secret), and carries the session in
   state: `{"at": "show", "session": ...}`. The next calls read the id
   back from `DEALS_APP_ID`. Each check (`"checked": true`) asks Deals, with a
   four-second timeout, whether the token has been shown, and answers
   the same Wait until it has, or when Deals cannot be reached;
   then a Form for `token`, which the next call tries against Deals
   before keeping it and answering Done.
4. **Requirements.** With no token: one unmet row, Fix `setup`. With
   one: the token works (one request, four seconds), and the scopes
   are this version's, or an unmet row whose Fix is `rescope`.
5. **The Fix.** `rescope` is a flow of its own: a Form for a new key,
   then one call that updates the app's scopes and installs it again,
   with progress, and Done.
6. **Uninstall.** Removes the app in Deals with its own token; an app
   already gone, or a token Deals no longer takes, is done.

Run it against the stub before you change it:

```
cd /files/skills/build-an-integration
T=scripts/try.py
S="python3 scripts/deals_stub.py 8765"
A=DEALS_API=http://127.0.0.1:8765

# Install: the Form, the build with its stages, what is kept, Done.
python3 $T --stub "$S" --env $A template-setup setup --answer setup_key=sk-test-1

# A key Deals refuses, then one it takes.
python3 $T --stub "$S" --env $A template-setup setup --answer setup_key=wrong --answer setup_key=sk-test-1

# The token only on the app's page: the owner leaves at the Wait after
# one check, and starts again, which takes up the app the first run made.
python3 $T --stub "$S --token-on-page" --env $A --every 0 --checks 1 template-setup \
  setup --answer setup_key=sk-test-1 setup --answer setup_key=sk-test-2 --answer token=app-token-0001

# Installed, with an app built before a version needed its scope: the
# requirements, the Fix, the requirements again, an uninstall asked twice.
python3 $T --stub "$S --installed --scopes ''" --env $A --field token=app-token-0001 --field app_id=app_1 \
  template-setup requirements setup rescope --answer setup_key=sk-test-3 requirements uninstall uninstall

# Before anything is kept, as integration_validate asks.
python3 $T --env $A template-setup requirements
```

The stub's first lines say what it answers. Copy it to
`/files/background/` as a start for your service's.

## Sketch: an app made from a manifest

A service whose app is created from a manifest the owner's browser
posts, then installed on the owner's account. GitHub Apps are made this
way, and Kivali's own GitHub integration does what follows. The owner
names the account, approves the app on the service's page, installs it
there, and copies nothing.

```json
"connection": {
  "method": "setup",
  "setup": {
    "summary": "Creates a private app on your Forge account that may read and write code and pull requests, and installs it on the repositories you choose.",
    "hosts": ["forge.example"],
    "fields": {
      "account": { "title": "Account", "words": "An organization's name on Forge, or leave it blank for your personal account." }
    },
    "keeps": {
      "app_id": { "words": "The app's id", "secret": false },
      "slug": { "words": "The app's name in Forge's addresses", "secret": false },
      "private_key": "The app's private key, which signs a token for each call",
      "installation_id": { "words": "Where the app is installed", "secret": false }
    }
  }
},
"hosts": ["api.forge.example"]
```

Split `answer.callback` first: the bare callback
(`https://team.example/connect/callback`) and its state `<s>`. Forge
passes a `state` on the new-app and installation pages' addresses
through to the address it sends the browser back to.

1. Form `account`, with `"state": {"at": "form"}`. Next call: answer a
   Go that posts the manifest to the new-app page for that account, the
   state on the page's address and the bare callback as both
   `redirect_url` (where Forge sends the code) and `setup_url` (where
   it sends the browser after an installation), the app named "Kivali"
   and `answer.team`, cut to the length Forge takes:

   ```json
   {"step": {"go": {"words": "Create the app on Forge: choose Create app. Forge may ask you to change the name first.",
                    "url": "https://forge.example/organizations/acme/settings/apps/new?state=4f0c9e2a7b1d4c3e8a6f5b2d1c0e9f8a",
                    "post": {"manifest": "{\"name\":\"Kivali Acme\",\"url\":\"https://team.example\",\"redirect_url\":\"https://team.example/connect/callback\",\"setup_url\":\"https://team.example/connect/callback\",\"setup_on_update\":true,\"public\":false,\"hook_attributes\":{\"active\":false},\"default_permissions\":{\"contents\":\"write\",\"pull_requests\":\"write\"},\"default_events\":[]}"}},
             "state": {"at": "make"}}}
   ```

   For the personal account the page is `/settings/apps/new`. Where the
   callback is plain `http` (a team on the owner's computer), the words
   say Forge may refuse it ([Go](#go)).
2. Forge sends the browser back with `?code=...&state=<s>`; the next
   call's `answer.return` is `{"code": ["..."], "state": ["..."]}`.
   Trade the code at once (it works once, within the hour):
   `POST https://api.forge.example/app-manifests/<code>/conversions`,
   with no credential, which answers the app's id, slug and private
   key. A code Forge does not take fails the flow with words saying to
   start again: nothing has been kept yet. Keep `app_id`, `slug` and
   `private_key`, and answer the next Go in the same call.
3. A Go to the app's installation page, the state on its address, and
   `"stateless": true`:
   `https://forge.example/apps/<slug>/installations/new?state=<s>`.
   The owner picks the repositories, and Forge sends the browser to the
   app's `setup_url` with `installation_id` and `setup_action`. That
   address is the app's own setting, so the return may come without
   the state: hence `stateless`, which nothing else here needs.
4. Ignore the returned `installation_id`: anyone could make the
   browser load the setup address with one. List the app's
   installations with a token signed by the kept key. One: keep its id
   and answer Done. None yet: the installation Go again, or, where the
   return says `setup_action=request` (an organization's owners must
   approve it), a Wait checked until it is installed, naming the
   installation page without the state: the slug is kept and not
   secret, so the address shows as it is, and an owner who left that
   page before installing finishes there.

Requirements: the key signs a token; the app is installed (Fix: the
installation Go and step 4 again); its permissions are this version's
(Fix: a Go, `no_return`, to the page where they are granted, then a
Wait checked until the installation has them). Uninstall: delete the
installation with the app's token; one already gone is done.

## Sketch: an app made by API, then a sign-in

A service whose OAuth app can be created through its API with an admin
key: setup makes the app, keeps its client id and secret, and hands
over to Kivali's browser sign-in, which then signs in, renews and
revokes as for any sign-in.

```json
"connection": {
  "method": "setup",
  "authorization_endpoint": "https://desk.example/oauth/authorize",
  "token_endpoint": "https://desk.example/oauth/token",
  "revocation_endpoint": "https://desk.example/oauth/revoke",
  "scopes": ["tickets.read", "tickets.write"],
  "credential_keys": { "access_token": "DESK_TOKEN" },
  "setup": {
    "summary": "Registers an app for Kivali in your Desk account with an admin key, then has you sign in with it. The admin key is used once and kept nowhere, and the app stays in Desk until an admin removes it.",
    "hosts": [],
    "fields": {
      "admin_key": { "title": "Admin key", "words": "An admin API key, from Admin, API, Keys, in Desk.", "secret": true, "setup_only": true }
    },
    "keeps": {
      "client_id": "The app's client id",
      "client_secret": "The app's client secret"
    },
    "then": "browser"
  }
},
"hosts": ["api.desk.example"]
```

1. Form `admin_key`.
2. One call: create the app with `POST https://api.desk.example/v1/oauth-apps`,
   its redirect address the bare callback (`answer.callback` without
   its query: Kivali's sign-in sends the state as OAuth's `state`), its
   scopes the ones the manifest's sign-in asks for; answer
   `{"done": true, "keep": {"client_id": ..., "client_secret": ...}}`.
3. Kivali's browser sign-in takes the flow on: a Go to the
   authorization endpoint with the kept app, back to the callback, the
   code traded with PKCE, the tokens kept. The server reads the token
   from `kivali/credential` on every call
   (SKILL.md, A sign-in connection: `credential()` in the template).

The app knows one redirect address, the one setup gave it; a sign-in
from another of the team's addresses is refused by the service. The
`sign_in` requirement (Kivali's own) asks the owner to sign in again
when the service stops taking the sign-in. Uninstall revokes the
tokens at `revocation_endpoint`; removing the app needs an admin key, which setup keeps
nowhere, so `kivali/uninstall` answers `{}` and the summary says the
app stays in Desk until an admin removes it.

## Try it as Kivali calls it

- `setup [FLOW]` runs a flow (`setup`, or a Fix's name) as Kivali's
  runner does: `kivali/setup` call after call with the answer Kivali
  builds, each step printed as the owner's page shows it (secrets as
  `[secret]`), each progress notification as it comes, what each step
  keeps, the server started again with it. A Form's fields come from
  `--answer KEY=VALUE`, in order: give a key twice to answer two Forms
  that ask for it (a refused answer, then a good one); the last is used
  for any Form after. A field with no `--answer` is asked for on the
  terminal, or, with none, left blank, as the page sends a field left
  blank, with a note saying so. A Go's return comes from `--return QUERY`, one
  per Go (`--return 'code=abc'`, or `--return 'error=access_denied'`
  for the owner turning it down); a return without a state gets the
  one Kivali gave the callback, unless the Go is `stateless`. A Wait is
  checked every `--every` seconds (0 in a script) until it changes or
  `--checks` checks have run. Without the answer a step needs, once a
  Wait's checks have run, or at a Form asked again after the same
  answer, the flow stops there, as when the owner leaves it: an install
  that stops before Done forgets what it kept, as Kivali does, and a
  `setup` after it in the same run starts again against what the stub
  holds.
- `requirements` reads the rows as the page does, held to what Kivali
  keeps of them, and says which flow each unmet row's Fix runs.
- `uninstall` sends `kivali/uninstall` as Uninstall does.

What a `problem:` line from a setup request means:

| `problem:` | Kivali |
| --- | --- |
| Kivali fails the flow: <words> | fails the flow with those words; the owner reads them under "Setup did not finish" |
| the return's state is none the flow gave | finds no flow: the browser lands on the integrations list and setup is not called |
| kivali/setup did not answer within 300 seconds | fails the flow: "setup did not finish its step within 5 minutes" |
| kivali/requirements answered rows Kivali cannot read | shows one unmet row, "<Integration> could not check its requirements" |
| the row <id> is left out | leaves that row out and logs it |
| the integration gave more than 20 rows | shows the first 20 |
| kivali/requirements did not answer within 5 seconds | shows the unmet row with "it did not answer within 5 seconds" |
| Kivali sends kivali/<x> only to a bundle whose method is setup | never sends it to this bundle |

## Before you publish a setup bundle

Besides SKILL.md's Before you publish:

- `try.py` shows no problem for:
  - the install, start to Done, and each other path through it (the
    fallback, each answer the service refuses, a Go the owner turns
    down);
  - a flow left at each step and started again, making nothing twice
    on the service;
  - `requirements` with no connection (no request to the service, at
    once), met, and not met, each unmet row's Fix run to Done and the
    requirements read again;
  - `uninstall`, and `uninstall` again once the install is gone.
- It answers the three setup requests, not -32601; every word the owner
  reads keeps to [Words the owner reads](#words-the-owner-reads); no
  value the owner must read is kept or carried in state.

## When a setup fails

| The page or the log says | The cause |
| --- | --- |
| <Integration>'s setup tried to send your browser to / named a page on / asked for / tried to keep ..., which its manifest does not declare | A step outside [the declarations](#the-declarations): fix the step, or declare it in a new version. |
| <Integration>'s setup answered with a step Kivali cannot read | A key or a type outside [the step's shape](#a-flow-call-by-call). |
| setup did not finish its step within 5 minutes | A call that waits on the service too long; split it with a Wait. |
| <Integration>'s server does not answer kivali/... ; its author must add it | The server answers that request -32601. |
| <Integration> is not running, so its setup cannot go on | The server did not start within two minutes; its log says why. |
| <Integration> is not running, so its own flows cannot run / so it cannot undo its installation | The server is down; a Fix or Uninstall waits for it, or the owner switches it off to uninstall. |
| <Integration> could not check its requirements: ... | `kivali/requirements` answered an error, rows without `id`, `title`, `words` or `met` or with another key, or nothing within five seconds. |
| <Integration> stopped before it answered | The server exited, or Kivali stopped it (a line that is not JSON-RPC), during the request; its log says which. |
| <Integration> failed and gave no reason | A JSON-RPC error with no message: give the owner's words in `message`. |
| the service sent the browser back without its state | The return lost the callback's state: pass the state where the service passes it through ([Coming back](#coming-back)), or mark the Go `stateless` where the service returns to an address of its own settings. |
| [secret] in a page address or the step's words, or in an agent's answer | A value kept under a secret keep, or carried in state, shows there: keep an identifier the service shows publicly under a keep declared `"secret": false`, and read it back from the connection rather than carry it ([State and keep](#state-and-keep)). |
