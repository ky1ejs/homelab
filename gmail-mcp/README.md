# gmail-mcp

One Gmail account as a remote MCP server for claude.ai, with the one thing the
hosted Gmail connector cannot do: **send from a verified "Send mail as" alias.**

The hosted connector always sends from the account's default address. Gmail's
API will send from any alias in `users.settings.sendAs` if the `From` header
names it, so this server exists to set that header, and to refuse every
address Gmail would otherwise silently replace.

- **Five tools**, all plain text: search, read, list aliases, draft, send.
- **No telemetry, by construction.** Every outbound request goes through an
  allow list of four hosts ([`egress.go`](egress.go)). There is nowhere for
  telemetry to go.
- **Same shape as [`vault-mcp`](../vault-mcp/)**: Go, a WorkOS AuthKit resource
  server, Tailscale Funnel, no published ports.

## Tools

| Tool | Does | Notes |
|---|---|---|
| `search_threads` | Gmail search syntax; subject, last sender, date, snippet, unread | 1 to 25 results, paged |
| `read_thread` | Every message: headers, plain-text body, attachment names | HTML is flattened; bodies cut at 20,000 characters |
| `list_send_as` | Aliases, which is default, and whether each is verified | Read-only |
| `create_draft` | A plain-text draft, optionally a reply, optionally `from` an alias | Sends nothing |
| `send_message` | The same, sent now | Cannot be undone |

`to`, `cc` and `bcc` take bare addresses only (`a@example.com`, not
`Alice <a@example.com>`). A display name is where header injection hides, and
a bare address is all a recipient field needs. Replies take
`reply_to_message_id`; the server sets `In-Reply-To`, `References`, the thread
id and a `Re:` subject from the original, so the reply lands in the thread.

No attachments, labels, archive or delete. The hosted Gmail connector already
does those; this server adds only what it cannot.

## The send-as rule

On every draft and every send, before anything is built:

1. Read `users.settings.sendAs` fresh. Not cached, so an alias you remove in
   Gmail stops working on the next call.
2. No `from`: use the account's default address.
3. `from` given: it must match an entry exactly (case-insensitively) **and**
   be the primary address or have `verificationStatus` `accepted`.
4. Anything else is refused with a message naming the verified aliases, and
   **no draft or send request is made.** `TestRefusedFromNeverReachesGmail`
   asserts that.

Step 4 is the point. Gmail does not reject a `From` it does not recognise: it
rewrites it to the default and sends anyway. A server that passed `from`
through unchecked would "work" while sending from the wrong address.

The `From` header is built from the alias's own display name in Gmail, never
from anything the caller sends.

## Trust boundary

This server reads mail written by strangers and can send mail as you. In
claude.ai it sits in the same conversation as `vault-mcp` and web access, so
all three legs (private data, untrusted content, egress) are present. That is
already true of the hosted Gmail connector, which reads and sends too. **What
this server adds is send-as**, not a new leg.

What that means in practice:

- **Set `send_message` to "Ask" in claude.ai's tool permissions.** A message
  that says "forward this to …" is content, and a confirmation you read is the
  control that stops it. `create_draft` is the safer default for anything
  composed from mail you did not write.
- The tool annotations mark `send_message` destructive and open-world, so a
  client that honours them asks by itself. Do not rely on it.
- The server's instructions tell the client that mail content is information,
  never instructions. That is advice to the model, not a control.
- The two Claude Code agents never see this connector: both set
  `disableClaudeAiConnectors`, which covers every account connector, this one
  included. See
  [`DECISIONS.md`](../obsidian-vault/DECISIONS.md#the-connectors-nobody-configured),
  and confirm with `/mcp` in each after adding it.

What the server itself guarantees, and tests:

| Guarantee | Test |
|---|---|
| An unverified or unknown `from` never reaches Gmail | `TestRefusedFromNeverReachesGmail` |
| A token for `vault-mcp` (same tenant) does not open mail | `TestVerify` "vault audience" |
| A subject not on the allow list is refused | `TestVerify` "other subject" |
| A recipient or subject cannot add a header | `TestBadRecipientsRefused`, `TestBuildRefusesHeaderBreaks` |
| A crafted thread or message id cannot reach another endpoint | `TestIDsCannotSteerThePath` |
| No request leaves for a host not on the list, or over plain http | `TestEgressAllowList` |
| The refresh token is never on disk in plaintext | `TestTokenStoreRoundTrip` |
| `-login` refuses another account, and revokes that grant | `TestFinishRefuses` |
| Blank gating config refuses to start | `TestLoadConfigFailsClosed` |

### What the log records

One line per tool call: the tool, the WorkOS subject, counts, and whether a
reply or an alias was used. **Never** a query, an address, a subject or a body.
A refused call logs why (`send-as` or `input`), and an unusable Google token
logs an error naming the cause.

## How this authenticates

Two separate credentials, for two separate questions.

**Who may call this server: WorkOS AuthKit**, the same tenant as `vault-mcp`,
and the same code ([`oauth.go`](oauth.go) is `vault-mcp/oauth.go` with one
change, below). An RFC 9728 metadata document, a `401` with a
`resource_metadata` challenge, and token checks for signature, issuer, expiry,
**audience** and **subject**.

- **Audience matters more here than in `vault-mcp`.** Two resources share one
  tenant, so without the audience check a token minted for the vault would open
  your mailbox. `OAUTH_RESOURCE` must be this connector's URL.
- **Subject is pinned, with no escape hatch.** `vault-mcp` has
  `OAUTH_ALLOW_ANY_SUBJECT=1`; this server does not. There is no legitimate
  second user of one person's mailbox, so the switch would only ever be a
  mistake waiting to be made.

**How this server reaches Gmail: a Google refresh token**, obtained once by
`gmail-mcp -login` and sealed on disk with AES-256-GCM under `GMAIL_TOKEN_KEY`.
The sealed file is bound to `GMAIL_ACCOUNT` as additional authenticated data,
so it cannot be relabelled for another mailbox. The server refuses to start
without a token it can open. If Google stops honouring it (revoked, password
changed, Testing-mode expiry), the server deletes it and every tool answers
"run `gmail-mcp -login` again".

Google's consent is deliberately **not** on the internet-facing server. There
is no `/oauth/callback` on the Funnel hostname; the callback is a loopback
address that only exists while `-login` runs. See
[`DECISIONS.md`](../obsidian-vault/DECISIONS.md#a-gmail-connector-for-send-as).

Scopes: `openid`, `email` (so `-login` can check which account consented),
`gmail.readonly` and `gmail.compose`. `gmail.compose` covers drafts and
sending; nothing here needs `gmail.modify` or full `mail.google.com`.

## Setup

### Google Cloud

1. Create a project and enable the **Gmail API**.
2. **OAuth consent screen**: if the mailbox is on Google Workspace, choose
   **Internal**. Internal apps skip verification and their refresh tokens do
   not expire on a schedule. If it is a personal account, External is the only
   choice; in **Testing** status Google expires refresh tokens after **7 days**,
   so you would run `-login` weekly. Publishing an unverified External app
   avoids that at the cost of an "unverified app" warning on consent. (Not
   verified here which applies to this mailbox; check before choosing.)
3. **Credentials -> Create OAuth client -> Desktop app.** Desktop clients accept
   any loopback redirect, which is what lets `-login` avoid registering a public
   one. Copy the id and secret into `.env`.

### Tailscale

Same as [`vault-mcp`](../vault-mcp/README.md#in-the-tailscale-admin-console),
with its own tag, so the two connectors are granted, audited and revoked
separately:

```jsonc
"tagOwners": { "tag:gmail-mcp": ["autogroup:admin"] },
"nodeAttrs": [
  { "target": ["tag:gmail-mcp"], "attr": ["funnel"] }
]
```

Then an auth key tagged `tag:gmail-mcp`, not ephemeral.

### WorkOS

The existing tenant. In **Connect -> Configuration**, add
`https://gmail-mcp.<tailnet>.ts.net/mcp` as a second **Resource Indicator**.
Leave the vault's as the default.

That is the one assumption here not tested against the real service: that
claude.ai sends the `resource` parameter, so WorkOS issues a token whose
audience is this URL rather than the default. If it does not, the token's
audience is the vault's, this server refuses it, and the connector fails
closed with a `401` rather than opening.

`OAUTH_ALLOWED_SUBJECTS` is the same `user_...` id as `vault-mcp`'s.

### On the NAS

```sh
cd /share/CE_CACHEDEV4_DATA/homelab/gmail-mcp
cp .env.example .env && chmod 600 .env    # every key; GMAIL_TOKEN_KEY from: openssl rand -hex 32
mkdir -p /share/CE_CACHEDEV4_DATA/gmail-mcp/{data,ts-state}
chown -R 1002:100 /share/CE_CACHEDEV4_DATA/gmail-mcp
docker compose pull
```

### Signing in

Once, before the first `up`, and again whenever the server says so.

On the Mac, forward the login port to the NAS's loopback:

```sh
ssh -L 8765:127.0.0.1:8765 kyles-nas
```

In that SSH session:

```sh
cd /share/CE_CACHEDEV4_DATA/homelab/gmail-mcp
docker compose run --rm -p 127.0.0.1:8765:8765 gmail-mcp /usr/local/bin/gmail-mcp -login
```

It prints a Google URL. Open it on the Mac, consent as `GMAIL_ACCOUNT`, and
leave every box ticked. Google redirects to `http://127.0.0.1:8765/callback`,
which the forward carries to the container, and the terminal reports a sealed
token saved.

If the forward is not running, the browser shows a page that cannot load. Copy
that page's full address into the terminal instead; it carries the same code.

`-login` refuses and revokes the grant if you consent as a different account or
untick a Gmail scope, and it writes no file. It times out after ten minutes.

Then:

```sh
docker compose up -d
docker compose logs -f gmail-mcp-funnel   # prints the public hostname
```

Or from the repo root: `bin/homelab deploy gmail-mcp`, `bin/homelab status gmail-mcp`
and `bin/homelab url gmail-mcp`.

### Confirm auth is enforced

```sh
# Expect 401, and a challenge naming the metadata document.
curl -si -X POST https://gmail-mcp.<tailnet>.ts.net/mcp | grep -i -e '^HTTP' -e www-authenticate

curl -s https://gmail-mcp.<tailnet>.ts.net/.well-known/oauth-protected-resource
```

### In claude.ai

**Settings -> Connectors -> Add custom connector**, URL
`https://gmail-mcp.<tailnet>.ts.net/mcp`, client id and secret empty. Then set
`send_message` to **Ask**.

Check in this order, each step before the next:

1. `list_send_as` shows the alias as verified.
2. `create_draft` from the alias, to yourself. Open the draft in Gmail and
   check the From line.
3. `create_draft` from an address that is not an alias. Expect a refusal, and
   no draft in Gmail.
4. Only then `send_message` from the alias, to yourself.

## What is not tested

- **No container has been built.** The Dockerfile mirrors `vault-mcp`'s, and
  the Go build and tests pass, but no image has run.
- **No real Google or WorkOS call has been made.** Everything above is tested
  against stand-ins of each API. The first real `-login` and the first real
  connector sign-in are the first end-to-end runs.
