# vault-mcp

The Obsidian vault as a remote MCP server, so Claude's **voice mode** can search
it, read from it and capture into it hands-free.

- **The agent stack** — [`../obsidian-vault/`](../obsidian-vault/)

> This is the only stack in the repo reachable from the internet. Read
> [Trust boundary](#trust-boundary) before deploying it.

---

## Why this exists

`vault-claude` is the surface for real work: a full Claude Code session with
grep, file tools, `CLAUDE.md` and skills, driven from the phone over Remote
Control. It has one gap — **the phone app's voice mode cannot drive it**.

Voice mode *can* call custom connectors, verified 2026-08-09 against the authless
DeepWiki server: voice discovered the connector, called it, and read the result
back. `obsidian-vault/DECISIONS.md` lists "Remote MCP server + Claude app" as a
rejected fallback, and that assessment still holds **as a replacement** — it
loses grep, file tools and skills. As a *second surface* it costs none of that,
because `vault-claude` is untouched and keeps all of it.

The trade it does make is real and is the subject of the rest of this file:
connector traffic is routed from Anthropic's cloud, not from the phone, so this
component needs a publicly reachable endpoint. Nothing else here does.

---

## Why Funnel and not Cloudflare

This was built on Cloudflare Tunnel first and moved to Tailscale Funnel on
2026-08-10, for one reason: **Cloudflare terminates TLS at its edge, so it can
read note contents in plaintext.** That is inherent to any TLS-terminating
reverse proxy or CDN, not a Cloudflare quirk — the proxy has to see the request
to route it. Anthropic's own [MCP tunnels](#mcp-tunnels-the-design-to-migrate-to)
run over Cloudflare and add a second, inner TLS layer terminated by a
certificate only the operator holds, specifically so that "Cloudflare cannot
read request or response payloads". We cannot do the same, because the inner
layer needs cooperation from the client and the client here is Claude's ordinary
connector fetcher.

Tailscale Funnel does not have the problem. It proxies the encrypted TCP stream
and **TLS terminates on this node**; Tailscale relays bytes it cannot read.

| | Cloudflare Tunnel | Tailscale Funnel |
|---|---|---|
| Who can reach it | Anthropic's IPs only, via a WAF rule | anyone who has the URL |
| Who can read your notes | **Cloudflare, at its edge** | nobody but the NAS |
| New dependency | a domain + a Cloudflare account | none — Tailscale already runs here |
| Access control | WAF rule **and** the token | the token alone |

The cost is real and worth stating plainly: Funnel does not forward the caller's
public IP ([tailscale#12972](https://github.com/tailscale/tailscale/issues/12972)),
so there is no edge allowlist and **the access token is the whole of access
control**. The judgement is that adding a party who can read personal notes is
worse than losing a second gate in front of a credential that expires. That is a
judgement, not a fact, and it is the thing to revisit if it ever feels wrong.

Switching back is a compose-service swap: set `MCP_ALLOWED_CIDRS` and
`MCP_CLIENT_IP_HEADER`, and point cloudflared at `vault-mcp:8080`. The server
supports both paths.

---

## Designing tools for voice

Voice is not text chat with a microphone attached, and the tools are shaped for
it deliberately.

| Constraint | Consequence |
|---|---|
| Results are **spoken** | Reads are capped at 8 KB, search returns a title plus one line. A tool that returns 4 KB of markdown produces a voice response nobody sits through. |
| A clarifying question costs a **whole turn** | `capture_note` takes one parameter. Notes resolve by title, so "Reading list" and "Reading list.md" are the same note — spoken input never includes the extension. |
| The model **cannot see** what it wrote | Every write returns the path it touched, so Claude can say "captured to Inbox" rather than guessing it worked. |
| Errors get **read aloud** | Expected failures return actionable tool output ("that text appears more than once, include surrounding lines"), not protocol errors the model can only apologise for. |

This came out of watching the DeepWiki test: its tools are built for text chat,
and over voice the interaction needed a disambiguation round trip before it
could do anything.

---

## Tools

| Tool | Does | Notes |
|---|---|---|
| `search_notes` | keyword search over titles and bodies | title matches first; plain substring scan, no index to keep in sync |
| `read_note` | read one note | truncates at 8 KB and says so |
| `list_notes` | list a folder | dotted entries hidden |
| `capture_note` | append a timestamped line to `CAPTURE_NOTE` | the main voice path — one parameter, no path to disambiguate |
| `append_note` | add to the end of any note | creates it if absent; never touches existing content |
| `create_note` | new note with a body | fails if it exists; refuses to overwrite |
| `edit_note` | replace anchored text | anchor must appear exactly once |

**There is no delete tool, and no tool takes whole-file replacement content.**

That distinction is the whole design. An `edit_note` that accepted full content
would be a delete with extra steps, so edits are anchored: `old_text` must match
exactly once, or the call fails. `edit_note` additionally refuses any edit whose
result would be blank — without that, passing a short note's entire body as the
anchor with an empty replacement empties it, and "no delete" would have been
decorative. `vault_test.go` asserts both properties.

---

## Writing safely alongside Obsidian

Two independent problems, two mechanisms.

**Torn files.** Every write is temp-then-rename on the same filesystem, fsynced,
with the directory fsynced after. A concurrent reader — the sync client — sees
either the old file or the new one, never a half-written one. This is the
mitigation `obsidian-vault/ARCHITECTURE.md#known-unresolved-risk` asks for, and
the reason this component is Go rather than a shell script.

**Lost edits.** Atomicity does nothing about read-modify-write: if `ob sync`
lands an edit made on the Mac between this server reading a note and writing it
back, the sync'd edit is silently discarded. So `append_note` and `edit_note`
re-read the file immediately before renaming and compare it byte-for-byte
against what they read; a mismatch aborts the write and returns a retryable
error. `create_note` re-checks absence the same way.

Byte comparison rather than mtime, because filesystem timestamp granularity can
be a full second — comfortably long enough to hide a lost edit. A window remains
between the check and the rename, and closing it entirely means locking the
vault against Obsidian itself, which is escalation step 2 in that document and a
much larger change than this. The idea is borrowed from
[cyanheads/obsidian-mcp-server](https://github.com/cyanheads/obsidian-mcp-server),
which reports byte counts on writes so an agent can spot a concurrent writer.

---

## Trust boundary

```mermaid
flowchart LR
    voice["Claude voice mode<br/>phone"] --> anth["Anthropic cloud"]
    anth -->|"TLS, relayed<br/>not terminated"| ts["Tailscale Funnel"]
    ts --> side["tailscale sidecar<br/>TLS terminates HERE"]
    side --> mcp["vault-mcp<br/>OAuth 2.1 resource server"]
    mcp --> vault[("/vault")]
    mcp --> snap[("/snapshots/vault.git")]
```

**One gate, one route.** `/mcp` accepts an OAuth 2.1 access token and nothing
else; anything without a valid one gets a detail-free 401 carrying a pointer to
the authorization server. Tokens expire, and access is revocable from a dashboard
— which is what no shared secret here could ever offer.

**The server refuses to start without `OAUTH_ISSUER`** unless `MCP_ALLOW_NO_AUTH=1`
is set explicitly. An unauthenticated start that silently worked would be your vault
readable by anyone who found the hostname.

**Assume the hostname is public.** Funnel gets its certificate from Let's
Encrypt, so `<host>.<tailnet>.ts.net` appears in Certificate Transparency logs.
Within minutes of the first certificate being issued, scanners were probing it
with TLS 1.0 handshakes and `spdy/2`. The hostname is not a secret and was never
a security control.

## How this authenticates

This is the most-revised decision in the stack, and the revisions were forced by
what the client can actually do rather than by what is best. Recorded in full
because the reasoning is not recoverable from the code.

### The route it takes now: OAuth 2.1, authorization server hosted

`OAUTH_ISSUER` points at a [WorkOS AuthKit](https://workos.com/docs/authkit/mcp)
tenant. This server is a **resource server only** and implements exactly three
things:

1. `/.well-known/oauth-protected-resource` — [RFC 9728](https://www.rfc-editor.org/rfc/rfc9728) metadata naming the authorization server
2. a `401` carrying `WWW-Authenticate: Bearer resource_metadata="…"`, which is what starts the flow
3. access-token validation — signature and issuer via the AS's published keys, **audience checked here**

Everything genuinely dangerous — `/authorize`, `/token`, dynamic client
registration, PKCE verification, redirect-URI validation, consent, refresh
rotation — belongs to the authorization server and is deliberately absent from
this binary.

**Audience is the check worth naming.** `go-oidc` validates the signature, issuer
and expiry; `aud` is enforced in `oauth.go` against `OAUTH_RESOURCE`. Without it,
any token the same tenant issued for any other resource would open this vault.
`oauth_test.go` mints real signed tokens against a stand-in authorization server
and asserts rejection for wrong audience, wrong issuer, expiry and garbage.

**`OAUTH_RESOURCE` must equal the connector URL exactly**, path included. The
client compares it against the URL you typed and rejects the document if they
differ.

### Pinning the subject

**Audience is not the last check, and for a while it was.** Signature, issuer,
expiry and audience together prove that AuthKit issued the token — to *somebody*.
They say nothing about **who**. `OAUTH_ALLOWED_SUBJECTS` is what makes this
server yours rather than the tenant's:

```sh
OAUTH_ALLOWED_SUBJECTS=user_01HBEQKA6K4QJAS93VPE39W1JT
```

That is the WorkOS user id from the AuthKit dashboard, and it is the `sub` claim
of every token issued to you.

This is not defence in depth. Until it existed, the only thing standing between
a stranger and the vault was a dashboard toggle, and the chain was short:

1. **The hostname is public.** Funnel needs a Let's Encrypt certificate, and
   every certificate is published to Certificate Transparency. `crt.sh` returns
   the connector hostname to anyone who searches the tailnet domain. It was never
   a secret and was never meant to be one.
2. **The metadata endpoint names the authorization server** — unauthenticated,
   correctly, because a client that cannot read it cannot begin the flow.
3. **AuthKit permits self-service sign-up by default**, with Google, Microsoft,
   GitHub and Apple as identity providers.
4. **Any resulting token satisfied every check this server made.**

So *"create the user you will sign in as, there is exactly one"* was a setup
convention, not a control. Nothing enforced the "one".

**A blank value refuses to start**, exactly as `MCP_ALLOW_NO_AUTH` does. To
genuinely honour every account on the tenant, `OAUTH_ALLOW_ANY_SUBJECT=1` says
so out loud. The empty value must never be the permissive one — that specific
mistake is [how this route was already wide open once](#two-schemes-that-shipped-and-were-removed).

**Also turn off self-service sign-up in the WorkOS dashboard.** Two independent
controls, because either alone is one misconfiguration from an open vault: the
allow list survives a dashboard change, and disabling sign-up means an attacker
never gets a token to present in the first place.

### Why not an authorization server of our own

That was the alternative, and it was rejected twice. Writing `/authorize`,
`/token`, DCR, PKCE verification and redirect-URI validation into an
internet-facing binary is several hundred lines of security-critical code, and
redirect validation and PKCE checks are precisely where hand-rolled OAuth fails.
A hosted AS reduces our share to ~150 lines of standards-boring glue.

WorkOS specifically because it is free to 1M monthly active users — this vault
has one — and because it supports **both DCR and CIMD**. Anthropic's client picks
CIMD when the AS advertises it and falls back to DCR otherwise; supporting both
means not betting on which. Auth0 gates DCR controls behind Enterprise, which is
exactly the trap that bet can spring. Clerk (50k free) is a fair second choice.

### Two schemes that shipped and were removed

Both of these worked. Both are gone, and the reasons are the interesting part.

**A static header token** (`MCP_TOKEN`) was the original design: a fixed
`Authorization: Bearer` value entered when adding the connector, compared in
constant time. It never survived contact with the client — `static_headers` is an
organization-admin beta and does not appear on a personal account, where the
dialog offers an OAuth Client ID and Secret and nothing else. This was written
down as the main risk to the design, and it materialised by never being available
rather than by being withdrawn.

**The same secret carried in the connector URL** (`MCP_PATH_SECRET`,
`/mcp/<secret>`) replaced it and ran the connector for a day. It was defensible
*here specifically*: in HTTPS the path is inside the TLS session, and TLS
terminates on the NAS, so Tailscale's Funnel relay forwards ciphertext no
intermediary can read. Anthropic saw it, exactly as it would have seen a header.
That safety was inherited entirely from choosing Funnel over Cloudflare — behind
a TLS-terminating proxy the secret is visible to that proxy.

What killed it was handling rather than cryptography: the connector settings page
displays the URL in plaintext, and URLs are what infrastructure logs by default.
Mitigations existed — `homelab url` redacted it, the 401 log line recorded no
path — but they were mitigations for a credential that should not have been
there.

**Why neither remains as a fallback.** Both were shared secrets that never
expired and could not be revoked. More decisively, an auth path nobody exercises
is where this server's one real hole was found: `withAuth` treated an empty
header token as "no auth configured, pass through", so configuring only the URL
secret left bare `/mcp` serving the whole vault unauthenticated on a public
hostname. Every unit test passed, including one written to assert that exact
property — it built its config by hand and never went through `loadConfig`. A
single live request found it.

Keeping a second scheme for break-glass would have meant keeping that class of
code correct forever with nobody testing it in anger. The cost of removal is
real: if WorkOS is unavailable, recovery is a code change and a rebuild rather
than an environment variable. That is the trade, made deliberately.

### What we learned, and would apply again

- **The client decides your auth, not you.** Three designs died to a dialog box.
  Check what the consuming client can actually send *before* designing around a
  documented feature.
- **"Beta" in vendor docs can mean "not for you"**, not "not yet stable".
- **A deny list is a property of the client, not the vault** — the same lesson in
  a different place. See [What voice cannot see](#what-voice-cannot-see).
- **Hosted auth is dramatically cheaper than it looks.** The expensive part of
  OAuth is being an authorization server. Being a resource server is three
  endpoints, and that distinction was worth about a day of avoidance.
- **Test the real config path.** Setting only a URL secret once left bare `/mcp`
  wide open on a public hostname while every unit test passed, including one
  written to assert that exact property — it built its config by hand. One live
  request found it.

### What the log records

Every tool call, and every refusal:

```
INFO  msg=tool          sub=user_01 name=read_note note=Fishing bytes=15
WARN  msg="tool denied" sub=user_01 reason=excluded
```

`sub` is the subject from the access token, so the trail answers **who** and not
only **what**. It is attached to the request context by the auth middleware and
read inside the tool handlers, which depends on the MCP SDK propagating request
context through to handlers -- an assumption about someone else's library, so
`oauth_test.go` asserts it end to end with a real signed token. If it ever stops
holding, the field degrades to `unknown` and nothing else breaks to tell you.

**Refusals are the higher-value half.** Successful reads are mostly your own
usage; an attempt to reach an excluded folder or escape the vault is the most
security-relevant thing this server can observe, and until 2026-08-12 it left no
trace at all -- the caller got a polite message and the operator got nothing.

**Content is never logged.** Not note bodies, and not search queries, which are
personal text in their own right. Paths and counts only.

```sh
homelab logs vault-mcp vault-mcp | grep 'tool denied'
```

### What this container deliberately cannot do

- **No credential volumes.** `home-sync` and `home-agent` are not mounted. This
  is the most exposed process in the repo and has no business holding the
  Obsidian login or the Claude OAuth token.
- **No writes to `.claude/`, `AGENTS.md` or `CLAUDE.md`**, at any depth. Those
  are the agent's standing instructions and its own tool policy. `vault.go`
  reimplements the deny list from `vault-claude-settings.json` — if the two ever
  drift, **this server becomes the way around the agent's permissions**, which
  is the failure mode that turns a convenience feature into a persistent
  compromise.
- **No reads or writes inside `MCP_EXCLUDE`.** Folders where material you did not
  write lands are invisible here, and only here — see
  [What voice cannot see](#what-voice-cannot-see).
- **No non-markdown paths.** A reference like `notes.txt` is refused rather than
  quietly becoming `notes.txt.md`.
- **No escaping the vault**, including via a symlink planted inside it.

### What it does not defend against

Anyone holding the token has the same access Claude does. It lives in `.env` on
the NAS (`0600`) and in the connector configuration. Rotating it is
`bin/homelab token rotate`, which writes a new one, recreates the container and
prints the header to paste into Claude — voice stays broken in the gap, which is
why doing it in one step matters.

**The client's tool surface is not ours.** Everything in this repo that denies a
tool denies it to `vault-claude`, where a settings file can say so. The consumer
of *this* server is claude.ai, which has web access and other connectors, and no
per-connector tool policy to set. That is why the vault is narrowed on the way
out instead — [What voice cannot see](#what-voice-cannot-see) — and why the
narrowing is a mitigation rather than a fix.

There is deliberately **no rate limiting**. With no client IP to key on, a
limiter would have to be global, which hands anyone who finds the hostname a way
to lock *you* out. A 32-byte random token is not guessable over HTTP; failed
attempts are logged instead.

---

## What voice cannot see

`MCP_EXCLUDE` lists folders and notes this server treats as absent: not
searched, not listed, not readable, not writable. It is the one control here
that is **not** mirrored from `vault-claude-settings.json`, and the asymmetry is
the point.

Prompt injection needs three things at once: private data, untrusted content,
and a way out. `vault-claude` removes the third. It reads the whole vault —
including the clipped articles and forwarded mail its own settings file names as
the risk — but `Bash`, `WebFetch` and `WebSearch` are denied, so an instruction
hidden in a clipping has nowhere to send anything.

This surface cannot remove that third leg, because the leg is not on this side
of the connection. The client is claude.ai, and its tool surface is not ours to
configure: web search, web fetch and every other connector on the account sit in
the same conversation as the vault. So the leg removed here is the second one —
keep the material you did not write out of the conversation that has a way out.

| | `vault-claude` (Remote Control) | `vault-mcp` (voice) |
|---|---|---|
| Reads | the whole vault | everything except `MCP_EXCLUDE` |
| Egress | denied: `Bash`, `WebFetch`, `WebSearch` | whatever claude.ai has; not ours to set |
| So an injected note | has nothing to exfiltrate through | is not in the context to begin with |

Each surface breaks a different leg. Neither breaks both, and neither needs to.

**This narrows the surface; it does not eliminate it.** Untrusted text is not
confined to the folder it arrived in — a forwarded email pasted into a project
note is still someone else's words, and this server will happily read it out.
The exclusion buys most of the reduction because most unvetted material lands in
one place. It is not a proof, and it should not be described as one.

### How entries are matched

| Spelling | Matches |
|---|---|
| `4. Inbox` | the folder and everything beneath it |
| `Private/Diary` | that note, asked for by title or as `Private/Diary.md` |
| `Private/Diary.md` | the same note |

Case-insensitively, because the vault is on a case-sensitive filesystem on the
NAS and a case-insensitive one on the Mac — a rule matching one spelling is
bypassable from whichever end folds case. Component-wise, so `4. Inbox` does not
quietly take `4. Inbox Archive` with it. Symlinks are resolved *before* the
check, on reads and on `list_notes` alike: a link inside the vault pointing into
an excluded folder is refused, or the exclusion is one `ln -s` from meaning
nothing. The list is comma-separated, so a folder with a comma in its name
cannot be expressed.

Writes are excluded along with reads, which matters more than it first looks.
`edit_note` reports whether its anchor was found once, never, or several times —
a read oracle over content the caller cannot otherwise see. An exclusion that
covered `read_note` and not `edit_note` would be decorative.

Two startup behaviours, both deliberate:

- **Fatal** if `CAPTURE_NOTE` resolves inside an excluded folder — or onto
  `CLAUDE.md`, or a non-markdown path. Capture is the main voice path and its
  first use will be from the car; that is the wrong moment to discover it.
- **A logged warning** if an entry matches nothing in the vault. A typo, or a
  folder renamed in Obsidian six months later, protects nothing while every
  other check still passes.

Removing an entry is one line in `.env` and a `docker compose up -d`. Nothing
else in the stack depends on the list.

---

## Snapshots

Voice writes commit **immediately**, as `Claude Voice <voice@vault.local>`.

This is not cosmetic. Snapshot commits bracket agent runs via Claude Code's
`SessionStart`/`Stop` hooks, and those hooks fire only for `vault-claude`. A
write arriving here fires nothing — so without an explicit commit, a note
captured by voice stays unversioned until `vault-sync`'s hourly backstop happens
to notice, and then lands attributed to a human. The distinct author keeps
`git log --author` an honest record of which surface wrote what:

```sh
git --git-dir=/snapshots/vault.git log --author="Claude Voice" --since=1.week --stat
```

Commits take the same `flock` as `snapshot.sh`, so this server, the session
hooks and the hourly backstop cannot interleave. A snapshot failure is **logged
and swallowed** — the note is already on disk, and turning a successful write
into a failed tool call would be the worse outcome.

---

## Setup

### On the Mac

Merge to `main`; CI builds and publishes `ghcr.io/<owner>/homelab/vault-mcp`.

Generate the token you will paste into Claude:

```sh
openssl rand -hex 32
```

### In the Tailscale admin console

1. **Access controls**: Funnel is opt-in. The tailnet policy needs the node
   attribute granted to this node's tag:

   ```jsonc
   "tagOwners": { "tag:vault-mcp": ["autogroup:admin"] },
   "nodeAttrs": [
     { "target": ["tag:vault-mcp"], "attr": ["funnel"] }
   ]
   ```

2. **DNS → HTTPS Certificates**: enable, or Funnel has no certificate to serve.
3. **Settings → Keys**: generate an auth key tagged `tag:vault-mcp`. Not
   ephemeral — the node must survive a recreate or the hostname changes, and the
   hostname is the connector URL.

### In the WorkOS dashboard

Free to 1M monthly active users; this vault has one.

1. Create an account and note the **AuthKit domain** (`https://<tenant>.authkit.app`) — that is `OAUTH_ISSUER`.
2. **Connect -> Configuration**, three things:
   - enable **Client ID Metadata Document (CIMD)** -- off by default
   - enable **Dynamic Client Registration** for clients that do not do CIMD yet.
     Anthropic's client prefers CIMD and falls back to DCR; enabling both avoids
     betting on which.
   - add the connector URL as a **Resource Indicator**, and set it as default.
     MCP clients send it as the `resource` parameter during the flow and WorkOS
     rejects values it does not recognise. It must match `OAUTH_RESOURCE`
     exactly. Easy to miss, and the failure it produces does not name it.
3. Create the user you will sign in as. There is exactly one — and the next two
   steps are what make that true, rather than merely intended.
4. **Turn off self-service sign-up.** It is *on* by default, with Google,
   Microsoft, GitHub and Apple. Left on, anyone who finds the connector
   hostname — which is public, via Certificate Transparency — can register and
   be issued a token this server would have honoured.
5. Copy your **user id** (`user_...`) from the Users list into
   `OAUTH_ALLOWED_SUBJECTS`. The server refuses to start without it. See
   [Pinning the subject](#pinning-the-subject) for why both steps exist rather
   than either one.

With CIMD or DCR the client registers its own redirect URI, so adding
`https://claude.ai/api/mcp/auth_callback` by hand should not be necessary. Add it
only if the consent flow complains about a redirect mismatch.

`OAUTH_RESOURCE` is the connector URL, exactly: `https://<host>.<tailnet>.ts.net/mcp`.

### On the NAS

```sh
cd /share/CE_CACHEDEV4_DATA/homelab/vault-mcp
cp .env.example .env && chmod 600 .env    # IMAGE, MCP_TOKEN, TS_AUTHKEY, paths, MCP_EXCLUDE
mkdir -p /share/CE_CACHEDEV4_DATA/vault-mcp/ts-state
docker compose pull
docker compose up -d
docker compose logs -f vault-mcp-funnel   # prints the public hostname once connected
```

Or, equivalently, from the repo root: `bin/homelab deploy vault-mcp`, then
`bin/homelab status` and `bin/homelab url`. See [Operating it](../README.md#operating-it).

Confirm the server is up and that auth is actually enforced:

```sh
docker compose exec vault-mcp vault-mcp -healthcheck && echo healthy

# From anywhere: expect 401 without the token, and a JSON-RPC reply with it.
curl -s -o /dev/null -w '%{http_code}\n' -X POST https://<host>.<tailnet>.ts.net/mcp

curl -s -X POST https://<host>.<tailnet>.ts.net/mcp \
  -H "Authorization: Bearer $MCP_TOKEN" \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}'
```

### In the Claude app

**Settings -> Connectors -> Add custom connector**, URL `https://<host>.<tailnet>.ts.net/mcp`.

With `OAUTH_ISSUER` set, leave the OAuth Client ID and Secret **empty** — the
client registers itself via CIMD or DCR. Adding the connector opens a WorkOS
sign-in once; after that, tokens refresh on their own.

If you are still on the interim path, paste the output of `bin/homelab url --secret`
instead, which carries the credential in the URL. **Treat that URL as a
password.** Cut over to OAuth by adding `OAUTH_ISSUER`/`OAUTH_RESOURCE`,
confirming the connector works, and only then clearing `MCP_PATH_SECRET` — both
routes authenticate independently, so there is never a window with no way in.

Verify in **text chat first** — that isolates a connector problem from a voice
problem — then open voice mode and try "what have I written about fishing?" and
"remember that I need 20lb braid".

Then confirm the write actually reached git, which is the one leg never exercised
until a real capture happens:

```sh
bin/homelab snapshots log --author='Claude Voice' --stat
```

---

## Invariants

Every one of these fails silently when broken.

| Invariant | What breaks if violated |
|---|---|
| The deny list in `vault.go` matches `vault-claude-settings.json` | This server becomes the bypass around the agent's tool policy |
| `MCP_EXCLUDE` covers wherever unvetted material actually lands | The clippings folder gets renamed, the entry stops matching, and voice starts reading text nobody wrote — startup warns, but only where someone reads the log |
| `MCP_EXCLUDE` gates writes as well as reads | `edit_note`'s anchor errors are a read oracle over notes the caller cannot open |
| `home-sync` and `home-agent` are never mounted here | The most exposed container in the repo gains the two credentials worth stealing |
| No port is published to the host | The listener becomes reachable from the LAN, bypassing nothing but widening exposure for no gain |
| `OAUTH_RESOURCE` equals the connector URL exactly, path included | The client rejects a metadata document whose `resource` differs, and the failure does not say so |
| The connector URL is registered as a Resource Indicator in WorkOS | The authorization flow fails on an unrecognised `resource`, with an error that does not name it |
| Token audience is checked against `OAUTH_RESOURCE` | Any token the same tenant issued for any other resource would otherwise open this vault |
| Nothing configured never means "no auth" | It once did, and left the endpoint a scanner finds serving the whole vault |
| `MCP_ALLOWED_CIDRS` stays empty while on Funnel | Every request is rejected for having no client address |
| `TS_STATE_HOST_PATH` persists across recreates | The node re-registers, the hostname changes, and the connector silently stops resolving |
| Writes stay temp-then-rename, with the pre-rename re-check | Reintroduces torn writes, or silently discards edits made in Obsidian |
| No tool accepts whole-file content | "No delete" stops meaning anything |

---

## MCP tunnels: the design to migrate to

[MCP tunnels](https://platform.claude.com/docs/en/agents-and-tools/mcp-tunnels/overview)
are what this stack would use if it could: outbound-only, no public endpoint, no
IP allowlisting, and an inner TLS layer so the transport provider cannot read
payloads. They are unusable here for one documented reason:

> MCP tunnels created through the Console are not available as connectors in claude.ai.

They reach Claude Managed Agents and the Messages API only, and voice mode lives
in claude.ai. They are also research preview, access by request, with no uptime
or continuity commitment. **If they ever reach consumer connectors, migrating is
the right move** — it removes the public hostname and the bearer-token-only
posture in one step.

---

## Open questions

- **`static_headers` is beta.** If it is withdrawn, the fallback is OAuth 2.1
  with DCR, which is a substantially larger thing to run. Worth watching.
- **Whether voice mode keeps calling custom connectors.** It does as of
  2026-08-09, but [claude-ai-mcp#146](https://github.com/anthropics/claude-ai-mcp/issues/146)
  was closed *as not planned* in April 2026 when it did not, so this is not a
  contract — it is current behaviour.
- **The token is the only gate.** Accepted deliberately (see
  [Why Funnel](#why-funnel-and-not-cloudflare)), but it means a leak is total
  until rotation. There is no mechanism here that would tell you it leaked.
- **Anthropic still sees vault contents**, as it already does through Remote
  Control. Funnel removes the *fifth* trust relationship, not the fourth — see
  `obsidian-vault/DECISIONS.md#open-questions`.
- **Whether excluding the ingest folders is the right cut.** It assumes unvetted
  material stays where it lands, which is true of clippings and not true of
  anything pasted into a note by hand. The alternative cut — excluding the
  *sensitive* folders instead, bounding what a successful injection could carry
  off rather than reducing the chance of one — is the same mechanism with a
  different list, and worth revisiting if the vault's shape changes.
- **Nothing here notices an injection attempt.** A note instructing the model to
  fetch a URL produces no signal on this side; the tool call happens in
  claude.ai, where this stack has no visibility at all.
