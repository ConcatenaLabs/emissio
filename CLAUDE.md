# Emissio

The Sequentia community rewards platform: contributors earn future mainnet Sequence tokens (SEQ)
for testnet work, competition wins and accepted security reports, and register the mainnet
address that receives their balance at launch.

A single Go binary with SQLite storage, no CGO. Server-rendered templates, no separate frontend
build. `README.md` documents the reward economics and the full feature set; this file covers the
mechanics of working on it.

Node and consensus conventions live in the
[`Sequentia`](https://github.com/GracedEternalKingCabbageMan/Sequentia) repo.

## Build, test, run

```sh
go build .
go test ./...
EMISSIO_LISTEN=127.0.0.1:8095 EMISSIO_DB=/path/to/emissio.db ./emissio
```

`go test ./...` covers address validation, auth, the submission and review lifecycle, prize and
report awards, CSRF, and double-credit protection. There is no CI, so run it before every PR.

Configuration is environment only — see the table in `README.md`. Nothing is read from a config
file. Deployment uses `deploy/emissio.service` and `deploy/install-on-box.sh`; the server pulls
this repo from GitHub and builds there. Never edit source on the server.

Admin creation reads the password from stdin so it never reaches shell history:

```sh
echo 'the-password' | ./emissio createadmin you@example.com
```

After editing the seeded task copy in `seed.go`, refresh an existing database with
`./emissio reseed-tasks`; rewards, caps and active flags set by admins are left untouched.

## The integrity model is the product

Emissio hands out something that will be worth money, so the anti-farming machinery is the part
that must not regress:

- **Every reward is a ledger entry and the balance is the sum of the ledger.** There is no
  mutable balance column to drift.
- **Unique indexes, not application checks, are what make double credits impossible.** One txid
  can only ever be evidence for one account; each referral pays exactly once; a social account
  can vouch for exactly one Emissio account, ever. If you touch the credit paths, keep the
  uniqueness in the schema.
- **Referral qualification is checked automatically whenever a credit lands** — both sides need
  50 SEQ earned from tasks, competitions or security reports, and referral and pre-sale credits
  do not count toward that.
- **At least one verified platform is required to receive the launch payout**, but not to earn.
  Blocking earning would cost onboarding; blocking payout is what actually matters.
- **Mainnet payout addresses are validated with a full checksum check**, and the error messages
  deliberately distinguish testnet (`tb1`), confidential (`sqb1`), Liquid and legacy formats.
  Sequentia uses Bitcoin's seed phrases, derivation and address formats, so an audited Bitcoin
  wallet is a valid way to generate one.

Manual ledger adjustments are always visible to the user. Keep it that way.

## Naming

The network is **Sequentia**. The token is named **Sequence**, ticker SEQ (tSEQ on testnet).
Never write "the SEQ chain" or use SEQ to mean the network.

## Working in this repo

- **Repository is public.** Never commit keys, seeds, credentials, `.env` files or tokens. The
  Telegram bot token is supplied through a systemd drop-in on the server, not through the unit
  file in this repo. The SQLite database is gitignored.
- **Commit author:**
  `GracedEternalKingCabbageMan <151803062+GracedEternalKingCabbageMan@users.noreply.github.com>`
- **Always open a pull request, then merge it yourself immediately.** The PR exists so the change
  and its reasoning are recorded, not because anyone is waiting to review it. There is no review
  process. If you are ever told to leave one specific PR open, that applies to that PR only and
  never becomes the default.
- PRs go against `main`, which is the remote default.
