# Emissio

Emissio is the Sequentia community rewards platform: an account-based web app where contributors earn the Sequence token (SEQ) for testnet work, competition wins, and accepted security reports. Balances are credited to an on-platform ledger and paid out at mainnet launch to a mainnet address the user registers; there is no mainnet today, and everything you interact with runs on the Sequentia public testnet.

Live instance: https://sequentiatestnet.com/emissio/

It is a single Go binary with embedded templates and SQLite storage (pure Go driver, no CGO).

## Where this fits in Sequentia

Sequentia is a Bitcoin sidechain for asset tokenization and decentralized exchange, built as a fork of Blockstream Elements 23.3.3. Emissio is a supporting service, not part of the protocol: it only talks to the chain read-only, through the block explorer's REST API, to sanity-check submitted transaction ids.

| Repo | One-liner |
|---|---|
| [`Sequentia`](https://github.com/GracedEternalKingCabbageMan/Sequentia) | The Sequentia node (`elementsd` fork of Elements 23.3.3): consensus, anchoring, proof of stake, open fee market, plus the canonical protocol documentation in `doc/sequentia/`. |
| [`sequentia-electrs`](https://github.com/GracedEternalKingCabbageMan/sequentia-electrs) | The electrs fork: Rust indexer + Esplora REST API for Sequentia and its Bitcoin testnet4 parent chain. |
| [`emissio`](https://github.com/GracedEternalKingCabbageMan/emissio) | Emissio: community rewards platform, earning Sequence tokens (SEQ) for testnet contributions. |

Protocol documentation lives in [`Sequentia/doc/sequentia/`](https://github.com/GracedEternalKingCabbageMan/Sequentia/tree/HEAD/doc/sequentia).

## Status

Working today on the live instance: accounts, the task catalog with explorer-backed txid checks, competitions, private security reports, social-account verification (Telegram, X, Reddit), referrals, mainnet payout-address registration, and the full admin review desk.

Not yet implemented:

- Email verification and password reset (admins can reset a password with the `createadmin` command).
- Automatic evidence verification beyond txid existence and confirmation. Fee-asset checks, blinded-output checks, and claim-code matching are manual review steps for now, even where task copy says "we check".
- The pre-sale purchase flow. The rules page announces future pre-sale windows and the ledger reserves a `presale` kind, but no sale mechanics exist by design.

This is testnet software supporting a testnet program. Reward balances are program commitments recorded in a database, not on-chain funds.

## For users: earning rewards

You need an account (email + password, 10 characters minimum). Every account gets a public claim code, which ties evidence to your account: you put it in social-media bios for verification, and it doubles as your referral code.

All amounts below are whole SEQ, credited to your ledger. On the testnet you work with tSEQ and testnet assets; the rewards themselves are Sequence tokens paid at mainnet launch. Launch reference price: 0.375 USD per SEQ. Program pool: 1,000,000 SEQ.

### Testnet tasks

Each task pays a fixed reward, most have a first-come cap, and you can have one submission per task (you may resubmit after a rejection). Most tasks require a txid as evidence; the app checks it against the testnet explorer and records an advisory note, then a human reviewer approves or rejects. A txid can only ever be evidence for one account.

The seeded catalog (`seed.go`; the live instance's admins can adjust rewards, caps, and active flags):

| Task | Reward | Cap |
|---|---|---|
| Make your first testnet transaction | 10 | 5,000 |
| Issue your own asset | 20 | 2,500 |
| Pay a fee in an asset other than tSEQ | 15 | 2,500 |
| Send an opt-in confidential transaction | 15 | 2,000 |
| Complete a SeqDEX swap | 25 | 1,500 |
| Complete a cross-chain swap with Bitcoin testnet | 40 | 750 |
| Complete a Lightning swap | 40 | 750 |
| Run a full node for a week | 50 | 1,000 |

Completing every task pays 215 SEQ; total task exposure across all users is 315,000 SEQ. Caps pay the first accounts whose evidence is approved, so the pool stays bounded.

### Competitions

Judged contests with ranked prize ladders (the seeded artwork competition pays 2,000 / 750 / 250 SEQ). One entry per account, editable until the competition closes. Admins assign places; each award is a ledger credit.

### Security reports

Private vulnerability reports with severity tiers; only the reporter and admins can see a report. Accepting a report credits the award. Default tiers: Low 250, Medium 1,000, High 4,000, Critical 13,500 SEQ; consensus-breaking or funds-loss bugs with a working proof of concept can be awarded up to 27,000 SEQ.

### Account verification (non-KYC)

Linking at least one verified social platform is required to receive the launch payout and to earn referral rewards. Earning itself is not blocked, so you can start on tasks immediately.

You can link a Telegram, X, and Reddit account, 15 SEQ per platform. As sybil resistance, the linked account must be at least two years old, and a social account can vouch for exactly one Emissio account, ever. Ownership proof is your claim code:

- Reddit: put the code in your profile's public description; ownership and account age are checked automatically.
- X: put the code in your bio; a reviewer checks it and the join date manually.
- Telegram: if the instance runs a verification bot, you send your code as a message to the bot, which proves ownership via your authenticated numeric ID; account age is estimated from the ID (Telegram does not publish creation dates). Without a bot, the app falls back to a public t.me bio check with reviewer-judged age.

All automatic checks are advisory; a human reviewer makes the final call.

### Referrals

Your claim code is also a referral link: `https://sequentiatestnet.com/emissio/r/<code>`. A referral pays 10 SEQ to each side, but only after both accounts hold a verified social platform and have each earned at least 50 SEQ from tasks, competitions, or security reports (referral and pre-sale credits do not count), and for at most 20 referrals per referrer. Qualification is checked automatically whenever a credit lands.

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
git clone https://github.com/GracedEternalKingCabbageMan/emissio
cd emissio
go build .
EMISSIO_LISTEN=127.0.0.1:8095 EMISSIO_DB=/tmp/emissio.db ./emissio
```

First run creates the schema and seeds the task catalog and the opening competition. Create an admin (the password is read from stdin so it never lands in shell history):

```
echo 'a-strong-password' | EMISSIO_DB=/tmp/emissio.db ./emissio createadmin you@example.com
```

Running `createadmin` for an existing email resets that account's password and makes it an admin. The other subcommand, `./emissio reseed-tasks`, refreshes the title, category, and body of the seeded tasks from the current `seed.go` copy in an existing database, leaving admin-tuned rewards, caps, and active flags alone.

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
| `EMISSIO_ESPLORA` | empty | Esplora API base URL for txid checks, e.g. `https://sequentiatestnet.com/api` (empty disables the check; submissions then go to manual review) |
| `EMISSIO_SECURE` | `0` | Set `1` behind HTTPS so cookies are marked Secure |
| `EMISSIO_TG_BOT_TOKEN` | empty | Telegram bot token (placeholder: `123456:ABC-...`); enables automatic Telegram ownership + ID-based age checks |
| `EMISSIO_TG_BOT_NAME` | empty | The bot's username (without `@`), shown in user instructions |
| `EMISSIO_REDDIT` | `https://www.reddit.com` | Reddit base URL (overridden in tests) |
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

The live instance runs as a systemd service behind Caddy on the sequentiatestnet.com host. `deploy/emissio.service` is the unit (binary + `/var/lib/emissio/emissio.db`, `EMISSIO_BASEPATH=/emissio`, local electrs as the esplora endpoint), and `deploy/install-on-box.sh` is the one-shot installer that builds, installs the unit, and adds the Caddy route:

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
