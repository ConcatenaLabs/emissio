# Sequentia Emissio

Emissio is the Sequentia community rewards platform: an account-based web app where contributors earn future mainnet Sequence tokens (SEQ) for testnet work, competition wins, and accepted security reports, and register the mainnet address that receives their balance at launch.

Live instance: https://sequentiatestnet.com/emissio/

## What it does

- **Accounts and ledgers.** Email and password accounts (argon2id, session cookies, CSRF protection). Every reward is a ledger entry; the balance is the sum of the ledger. Each account gets a public claim code used to tie on-chain evidence to the account.
- **Testnet tasks.** A catalog of tasks with per-task SEQ rewards, optional first-come caps, and one submission per account. Submissions carry a txid; the app checks it against the testnet explorer (esplora API) and records an advisory chain note. A human reviewer approves or rejects; approval credits the ledger atomically, with a unique index that makes double credits impossible.
- **Competitions.** Judged contests with ranked prize ladders (for example 2,000 / 750 / 250 SEQ). One entry per account, editable until close. Admins assign places; each award is a ledger credit.
- **Security reports.** Private vulnerability reports with severity tiers. Only the reporter and admins see a report. Accepting a report credits the award.
- **Referrals.** Every account's public code doubles as a referral code (`/r/<code>`). A referral pays 10 SEQ to each side, but only after both accounts have earned at least 50 SEQ from tasks, competitions or security reports (referral and pre-sale credits do not count), and for at most 20 referrals per referrer. Qualification is checked automatically whenever a credit lands; unique indexes make each referral pay exactly once. Together with txid uniqueness (one on-chain transaction can only ever be evidence for one account), this keeps automated farming uneconomical.
- **Account verification (non-KYC).** Users can link a Telegram, X and Reddit account, 15 SEQ per platform, as sybil resistance: the linked account must be at least two years old, and a social account can vouch for exactly one Emissio account, ever (unique index). Ownership is proven by putting the account code in the profile bio (Reddit ownership and age are checked automatically via `about.json`; X is reviewed manually). For Telegram, configure a bot (`EMISSIO_TG_BOT_TOKEN`, `EMISSIO_TG_BOT_NAME`): users send their code as a message to the bot, which yields their authenticated numeric ID, and account age is estimated from the ID against the community anchor table (Telegram does not publish creation dates). Without a token it falls back to a t.me bio check with reviewer-judged age. Registration IPs are recorded and surfaced in the admin user list with a shared-IP counter.
- **Mainnet payout address.** Users register a Sequentia mainnet address (bech32/bech32m, HRP `bc`, segwit v0 or v1), validated with a full checksum check and helpful errors for testnet (`tb1`), confidential (`sqb1`), Liquid, and legacy formats. A guide explains safe key generation; because Sequentia uses Bitcoin's seed phrases, derivation, and address formats, any audited Bitcoin wallet works.
- **Admin desk.** Review queues for submissions and reports, task and competition management, manual ledger adjustments (always visible to the user), and a CSV export of `(user, address, balance)` for the launch allocation.
- **Pre-sales.** The rules and home page announce future short pre-sale windows for account holders below the launch price; the ledger has a reserved `presale` kind. No sale mechanics are implemented yet by design.

## Reward economics

Launch reference price: 0.375 USD per SEQ. Program pool: 1,000,000 SEQ.

| Track | Amounts | Bounds |
| --- | --- | --- |
| Testnet tasks | 10 to 50 SEQ per task, 215 SEQ per user for the full track | Per-task caps of 750 to 5,000 completions; total exposure about 315,000 SEQ |
| Competitions | 500 to 4,000 SEQ prize pools | Created per event by admins |
| Security | Low 250 / Medium 1,000 / High 4,000 / Critical 13,500 SEQ | Up to 27,000 SEQ for consensus-breaking or funds-loss issues |

Amounts are whole SEQ. Caps pay the first accounts whose evidence is approved, so the pool is bounded even with thousands of participants.

## Running

Single Go binary, SQLite storage, no CGO.

```
go build .
EMISSIO_LISTEN=127.0.0.1:8095 EMISSIO_DB=/var/lib/emissio/emissio.db ./emissio
```

Configuration (environment variables):

| Variable | Default | Meaning |
| --- | --- | --- |
| `EMISSIO_LISTEN` | `127.0.0.1:8095` | Listen address |
| `EMISSIO_DB` | `emissio.db` | SQLite database path |
| `EMISSIO_BASEPATH` | empty | Path prefix when served under one, e.g. `/emissio` |
| `EMISSIO_ESPLORA` | empty | Esplora API base URL for txid checks, e.g. `https://sequentiatestnet.com/explorer/api` |
| `EMISSIO_SECURE` | `0` | Set `1` behind HTTPS so cookies are Secure |
| `EMISSIO_TG_BOT_TOKEN` | empty | BotFather token; enables automatic Telegram ownership + ID-based age checks |
| `EMISSIO_TG_BOT_NAME` | empty | The bot's username (without @), shown in user instructions |

Set the bot token via a systemd drop-in (`systemctl edit emissio`), not in the unit file in this repo.

First run seeds the task catalog and the opening competition. Create an admin (password is read from stdin, so it never lands in shell history):

```
echo 'the-password' | ./emissio createadmin you@example.com
```

After editing the seeded task copy in `seed.go`, refresh an existing database with `./emissio reseed-tasks` (rewards, caps and active flags set by admins are left untouched).

Deploy: see `deploy/emissio.service` for the systemd unit and reverse-proxy notes. Tests: `go test ./...` covers address validation, auth, the full submission and review lifecycle, prize awards, report awards, CSRF, and double-credit protection.

## Not yet implemented

- Email verification and password reset (admins can reset via `createadmin` for now).
- Automatic evidence verification beyond txid existence and confirmation (fee-asset checks, blinded-output checks, claim-code matching are manual review steps for now).
- Pre-sale purchase flow.
