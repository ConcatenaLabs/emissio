# Emissio

Emissio is the Sequentia community rewards platform: an account-based web app where contributors earn the Sequence token (SEQ) for testnet work, competition wins, and accepted security reports. Balances are credited to an on-platform ledger and paid out at mainnet launch to a mainnet address the user registers; there is no mainnet today, and everything you interact with runs on the Sequentia public testnet.

Live instance: https://sequentiatestnet.com/emissio/

It is a single Go binary with embedded templates and SQLite storage (pure Go driver, no CGO).

## Where this fits in Sequentia

Sequentia is a Bitcoin sidechain for asset tokenization and disintermediated exchanges, built as a fork of Blockstream Elements 23.3.3. Emissio is a supporting service, not part of the protocol: it only talks to the chain read-only, through the block explorer's REST API, to sanity-check submitted transaction ids.

| Repo | One-liner |
|---|---|
| [`Sequentia`](https://github.com/ConcatenaLabs/Sequentia) | The Sequentia node (`sequentiad`, a fork of Elements 23.3.3): consensus, anchoring, proof of stake, open fee market, plus the canonical protocol documentation in `doc/sequentia/`. |
| [`sequentia-electrs`](https://github.com/ConcatenaLabs/sequentia-electrs) | The electrs fork: Rust indexer + Esplora REST API for Sequentia and its Bitcoin testnet4 parent chain. |
| [`emissio`](https://github.com/ConcatenaLabs/emissio) | Emissio: community rewards platform, earning Sequence tokens (SEQ) for testnet contributions. |

Protocol documentation lives in [`Sequentia/doc/sequentia/`](https://github.com/ConcatenaLabs/Sequentia/tree/HEAD/doc/sequentia).

## What it does

Accounts, the task catalog with explorer-backed txid checks, competitions, private security reports, social-account verification (Telegram, X, Reddit), referrals, mainnet payout-address registration, and the admin review desk.

Three things it deliberately does not do, which a user or an operator needs to know:

- There is no email verification and no self-service password reset; an admin resets a password with the `createadmin` command.
- Evidence is checked automatically only for a transaction id's existence and confirmation. Fee-asset checks, blinded-output checks and claim-code matching are human review steps, and the task pages say so.
- There is no pre-sale purchase flow. The rules page describes pre-sale windows and the ledger reserves a `presale` kind, but no sale mechanics exist here.

This is testnet software supporting a testnet program. Reward balances are program commitments recorded in a database, not on-chain funds.

## For users: earning rewards

You need an account (email + password, 10 characters minimum). Every account gets a public claim code, which ties evidence to your account: you put it in a social-media bio or an X post for verification, and it doubles as your referral code.

All amounts below are whole SEQ, credited to your ledger. On the testnet you work with tSEQ and testnet assets; the rewards themselves are Sequence tokens paid at mainnet launch. Launch reference price: 0.175 USD per SEQ. Program pool: 32,285,714 SEQ.

### Testnet tasks

Each task pays a fixed reward, most have a first-come cap, and you can have one submission per task (you may resubmit after a rejection). A txid can only ever be evidence for one account.

### Proofs

A txid on its own proves that a transaction exists, not that the submitter made it, so every account has **proof decimals**: eight digits derived from its claim code (`proofTag` in `proof.go`), shown on the account page and substituted into task instructions. A transaction counts as evidence only if one of its transparent outputs carries an amount ending in exactly those decimals, for example `1.86718345` tSEQ. Any wallet can send an exact amount, and a stranger's transaction matches by accident with probability one in a hundred million per output.

On top of that, each task checks the property it is about, from the explorer's view of the transaction (`evidenceCheck`): the fee output's asset, outputs in two assets, a blinded output, a new issuance or a reissuance of an asset whose issuance the same account submitted, a tSEQ output of at least 40,000, the Bitcoin anchor of the containing block against a hash in the notes. The verdict is written into the submission's check note and shown in the review queue; a reviewer still approves, and can re-run the check.

Two tasks are proven by the node rather than by a transaction. With `EMISSIO_CLI` and `EMISSIO_DATADIR` set, the app reads the public node's peer list: **build from source** passes when a connected peer announces the client name `Emissio-<code>`, which only a rebuilt binary can carry; **run a node for a week** is judged from sightings the app records every ten minutes of peers whose user agent contains `emissio-<code>` (a `-uacomment`), and passes when a week's span shows the node in at least 80% of the slots.

Task instructions carry the user's own values where `{PROOF}` (proof decimals) and `{CODE}` (account code) appear in the seeded text.

The same binding applies outside tasks. A **competition entry** must carry the account code on the work or its page; the app looks for it in the link and in the linked page's text, notes what it found for the judges, and refuses a link another account already entered. A **security report** must start with the account code, inside the encrypted block when encrypted, so a report read long after it was sent still names its account; identical bodies from two accounts are flagged. The **bug-report task** reads the linked GitHub issue through the API (`EMISSIO_GITHUB_API`) and checks that the issue text contains the code.

The seeded catalog (`seed.go`; the live instance's admins can adjust rewards, caps, and active flags):

| Task | Reward | Cap |
|---|---|---|
| Make your first testnet transaction | 10 | 5,000 |
| Find the Bitcoin anchor of your transaction | 10 | 3,000 |
| Issue your own asset | 20 | 2,500 |
| Reissue an asset you created | 20 | 1,000 |
| Pay a fee in an asset other than tSEQ | 15 | 2,500 |
| Send two assets in one transaction | 15 | 2,000 |
| Send an opt-in confidential transaction | 15 | 2,000 |
| Bridge an asset into Sequentia with Compages | 30 | 1,000 |
| Bridge an asset back out with Compages | 40 | 500 |
| Build Sequentia Core from source (no txid; the peer list is the proof) | 30 | 500 |
| Stake tSEQ | 40 | 500 |
| Delegate your stake to a staking pool | 30 | 500 |
| Run a full node for a week (no txid; a week of peer sightings is the proof) | 50 | 1,000 |
| Report a bug (a confirmed public issue, no txid) | 25 | 400 |

Completing every task pays 350 SEQ; total task exposure across all users is 407,500 SEQ. Caps pay the first accounts whose evidence is approved, so the pool stays bounded.

### Competitions

Judged contests with ranked prize ladders (the seeded artwork competition pays 2,000 / 750 / 250 SEQ). One entry per account, editable until the competition closes. Admins assign places; each award is a ledger credit. The admin page for a competition edits its slug, title, body, prizes and closing date; a competition whose date has passed refuses new entries until the date is moved.

### Security reports

Private vulnerability reports with severity tiers; only the reporter and admins can see a report. Accepting a report credits the award. Default tiers: Low 1,000, Medium 6,000, High 30,000, Critical 150,000 SEQ. Every tier requires a defect in shipped code, a demonstrated security consequence and a reproduction a reviewer can confirm; bugs without a security angle go through the "Report a bug" task. Reporters can encrypt a report to the team's PGP key, served at `static/security-pgp.asc` with its fingerprint in `seed.go`; the private key is held by the review team off the server, and the admin views mark encrypted reports so they are decrypted locally rather than read in the browser. An exceptional finding (network-wide consequences, a truly novel technique, a working proof of concept) can be awarded up to 1,500,000 SEQ; 16,000,000 SEQ of the pool is reserved for security awards. The tier amounts, the ceiling and the reserve are constants in `seed.go`.

### Account verification (non-KYC)

Linking at least one verified social platform is required to receive the launch payout and to earn referral rewards. Earning itself is not blocked, so you can start on tasks immediately.

You can link a Telegram, X, and Reddit account, 15 SEQ per platform. As sybil resistance, the linked account must be at least two years old, and a social account can vouch for exactly one Emissio account, ever. Ownership proof is your claim code:

- Reddit: with `EMISSIO_REDDIT_TOKEN_SECRET` set, the user gets a signed token from the Emissio app installed on r/sequentia (the `emissio-reddit-verifier` repository), which Reddit authenticates, and pastes it into the account page; the token carries the username, the account creation date and the account code, and is verified here with the shared secret. Without that secret, the user puts the code in their profile description; with Reddit API app credentials the description and age are checked through the API, otherwise a reviewer checks by hand, since Reddit blocks unauthenticated requests.
- X: publish a post containing the code and submit its link. The handle is taken from the link, so the account locked by the one-account-ever rule is the one that published the code. The app fetches the post from X's keyless embedded-post endpoint (`EMISSIO_X`, default `https://cdn.syndication.twimg.com`) and checks that the author matches that handle (X shows a post under any handle in the URL), that the text contains the code, and the account age, derived from the author's numeric id, which encodes the creation time for accounts made since 2016 and predates that for smaller ids. A post already attached to another request is flagged in the queue.
- Telegram: if the instance runs a verification bot, you send your code as a message to the bot, which proves ownership via your authenticated numeric ID; account age is estimated from the ID (Telegram does not publish creation dates). Without a bot, the app falls back to a public t.me bio check with reviewer-judged age.

All automatic checks are advisory; a human reviewer makes the final call.

### Referrals

Your claim code is also a referral link: `https://sequentiatestnet.com/emissio/r/<code>`. A referral pays 10 SEQ to each side, but only after both accounts hold a verified social platform and have each earned at least 50 SEQ from tasks, competitions, security reports, or positive admin adjustments (referral and pre-sale credits do not count), and for at most 20 referrals per referrer. Qualification is checked automatically whenever a credit lands.

### Payout address

You register a Sequentia mainnet receiving address on your account page. Sequentia's transparent addresses use Bitcoin's own bech32/bech32m encoding, so a mainnet address starts with `bc1` (segwit v0 or v1), and any audited Bitcoin wallet can generate one; the in-app guide explains safe key generation. Validation does a full checksum check and gives specific errors for testnet (`tb1`), confidential (`sqb1`), Liquid, and legacy formats.

## The review model

Every reward flows through human review: task submissions, security reports, and verifications sit in admin queues, and competitions are judged by admins. Automatic checks (explorer txid lookup, Reddit `about.json`, the Telegram bot) only annotate the queue. Admin accounts are ordinary accounts with an admin flag, created from the server command line; there is no self-service admin signup.

Anti-farming, mechanically enforced in the database:

- Approval credits the ledger inside the same transaction as the status change, and partial unique indexes on the ledger make double credits impossible (one credit per submission, entry, report, verification, and referral).
- One transaction id can be evidence for one account only.
- One social account can verify one Emissio account, ever.
- Registration IPs are recorded and surfaced in the admin user list with a shared-IP counter.
- Manual admin ledger adjustments are always visible to the affected user.

## For integrators

Emissio has no public machine-readable API. It is a server-rendered HTML application: session cookies (HttpOnly, SameSite=Lax), a per-session CSRF token on every POST, and a strict same-origin Content-Security-Policy. The only machine-readable endpoint is the admin-only launch-allocation export at `/admin/allocations.csv` (columns: `user_id, email, mainnet_address, balance_seq, verified_platforms`).

Public routes, for orientation (see `routes()` in `main.go` for the full list including the `/admin/*` desk):

| Route | Purpose |
|---|---|
| `GET /` | Home: program overview and stats |
| `GET /tasks`, `GET /tasks/{slug}`, `POST /tasks/{slug}/submit` | Task catalog and evidence submission |
| `GET /competitions`, `GET /competitions/{slug}`, `POST .../enter` | Competitions and entries |
| `GET /security`, `POST /security/report` | Security program and private reports |
| `GET /guide`, `GET /rules` | Wallet/key-safety guide and program rules |
| `GET /account`, `POST /account/address`, `POST /account/verify` | Ledger, payout address, verifications |
| `GET /r/{code}` | Referral link (sets a 30-day cookie, redirects to registration) |
| `GET|POST /register`, `GET|POST /login`, `POST /logout` | Accounts (auth endpoints are rate-limited per IP) |

Emissio consumes one external API surface itself: an Esplora-compatible REST API (`GET <base>/tx/<txid>`) for txid checks, configured with `EMISSIO_ESPLORA`. The public Sequentia instance is `https://sequentiatestnet.com/api`.

## For contributors

### Build, run, test

Requirements: Go 1.24 or newer. No CGO, no C toolchain; SQLite is the pure-Go `modernc.org/sqlite` driver.

```
git clone https://github.com/ConcatenaLabs/emissio
cd emissio
go build .
EMISSIO_LISTEN=127.0.0.1:8095 EMISSIO_DB=/tmp/emissio.db ./emissio
```

First run creates the schema and seeds the task catalog and the opening competition. Create an admin (the password is read from stdin so it never lands in shell history):

```
echo 'a-strong-password' | EMISSIO_DB=/tmp/emissio.db ./emissio createadmin you@example.com
```

Running `createadmin` for an existing email resets that account's password and makes it an admin. The other subcommand, `./emissio reseed-tasks`, refreshes the title, category, and body of the seeded tasks from the current `seed.go` copy in an existing database, inserts any seeded task the database does not have yet, and deactivates the tasks listed as retired in `seed.go`, leaving admin-tuned rewards, caps, and active flags of the other existing tasks alone.

Tests:

```
go test ./...
```

`app_test.go` drives the full HTTP surface against a temporary database: registration, submission and review lifecycle, prize and report awards, referral qualification, verifications (with stubbed Reddit/Telegram servers), duplicate-txid rejection, base-path handling, CSRF, and anonymous-access denial. `bech32_test.go` covers address validation and formatting helpers.

### Configuration

All configuration is environment variables (`loadConfig` in `main.go`):

| Variable | Default | Meaning |
| --- | --- | --- |
| `EMISSIO_LISTEN` | `127.0.0.1:8095` | Listen address |
| `EMISSIO_DB` | `emissio.db` | SQLite database path |
| `EMISSIO_BASEPATH` | empty | Path prefix when served under one, e.g. `/emissio` |
| `EMISSIO_ESPLORA` | empty | Esplora API base URL for the transaction checks, e.g. `https://sequentiatestnet.com/api` (empty disables them; submissions then go to manual review) |
| `EMISSIO_POLICY_ASSET` | the testnet's tSEQ id | Asset id of tSEQ, for the fee and stake checks |
| `EMISSIO_GITHUB_API` | `https://api.github.com` | GitHub API base for the bug-report issue check (overridden in tests) |
| `EMISSIO_CLI`, `EMISSIO_DATADIR` | empty | `sequentia-cli` path and the public node's data directory; enables the peer-based checks and the ten-minute sighting poller |
| `EMISSIO_SECURE` | `0` | Set `1` behind HTTPS so cookies are marked Secure |
| `EMISSIO_TG_BOT_TOKEN` | empty | Telegram bot token (placeholder: `123456:ABC-...`); enables automatic Telegram ownership + ID-based age checks |
| `EMISSIO_TG_BOT_NAME` | empty | The bot's username (without `@`), shown in user instructions |
| `EMISSIO_REDDIT_TOKEN_SECRET` | empty | Secret shared with the Reddit app on r/sequentia that signs ownership tokens; enables the token flow for Reddit |
| `EMISSIO_REDDIT_CLIENT_ID`, `EMISSIO_REDDIT_CLIENT_SECRET` | empty | Credentials of a Reddit app (created at reddit.com/prefs/apps, type "script"); enables the automatic Reddit check. Reddit blocks unauthenticated requests, so without them Reddit requests go to manual review |
| `EMISSIO_REDDIT` | `https://www.reddit.com` | Reddit base URL, where tokens are issued (overridden in tests) |
| `EMISSIO_REDDIT_OAUTH` | `https://oauth.reddit.com` | Reddit API host, where profiles are read (overridden in tests) |
| `EMISSIO_X` | `https://cdn.syndication.twimg.com` | X embedded-post endpoint (overridden in tests) |
| `EMISSIO_TELEGRAM` | `https://t.me` | t.me base URL (overridden in tests) |

Keep real secrets (such as the bot token) out of the repo and out of unit files under version control; on a systemd host, use a drop-in (`systemctl edit emissio`).

### Repo layout

| File | Contents |
|---|---|
| `main.go` | Config, embedded templates/static, CLI subcommands, route table, security headers, base-path handling |
| `handlers.go` | All public HTTP handlers |
| `admin.go` | Admin desk handlers (queues, task/competition management, adjustments, CSV export) |
| `db.go` | Schema, migrations, and all SQL (ledger credits happen here, inside transactions) |
| `auth.go` | argon2id password hashing, sessions, CSRF, per-IP rate limiting |
| `bech32.go` | BIP-173/BIP-350 address validation for the payout address |
| `seed.go` | Program constants, security tiers, seeded tasks and competitions |
| `referrals.go` | Referral qualification logic |
| `verifications.go` | Social verification: platform definitions and Reddit/t.me checks |
| `telegram.go` | Telegram bot check and ID-based account-age estimation |
| `esplora.go` | Explorer txid check |
| `templates/`, `static/` | HTML templates and assets, embedded into the binary |
| `deploy/` | systemd unit and box install script |

### Data storage

One SQLite file (WAL mode, foreign keys on, a single connection to serialize writers). Tables: `users`, `sessions`, `tasks`, `submissions`, `competitions`, `entries`, `reports`, `ledger`, `verifications`. Every reward is a ledger row; a balance is `SUM(amount)` over the user's rows. Ledger kinds: `submission`, `entry`, `report`, `verification`, `referral`, `referral-welcome`, `adjustment`, and the reserved `presale`. Partial unique indexes on `(kind, ref_id)` are the double-credit guard.

### Deployment

The live instance runs as a systemd service behind Caddy on the sequentiatestnet.com host. `deploy/emissio.service` is the unit (binary + `/var/lib/emissio/emissio.db`, `EMISSIO_BASEPATH=/emissio`, local electrs as the esplora endpoint), and `deploy/install-on-box.sh` is the one-shot installer that builds, installs the unit, and adds the Caddy route. It assumes Go at `/root/toolchains/go/bin` and the box's canonical Caddy site block; on any other host, follow the steps in the header of `deploy/emissio.service` by hand. The route it adds:

```
redir /emissio /emissio/ permanent
handle_path /emissio/* {
    reverse_proxy 127.0.0.1:8095
}
```

Caddy's `handle_path` strips the `/emissio` prefix before proxying, but the app still needs `EMISSIO_BASEPATH=/emissio` to generate correct links; it strips the prefix itself if present, so both stripped and unstripped proxy setups work.

Contributions: PRs against `main`.

## License

MIT, see `LICENSE`.
