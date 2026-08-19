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
- **A credit lands inside the same transaction as the status change it follows**, and partial
  unique indexes on `(kind, ref_id)` are what make double credits impossible — one credit per
  submission, entry, report, verification and referral. One txid can only ever be evidence for one
  account, and a social account can vouch for exactly one Emissio account, ever. If you touch the
  credit paths, keep the guard in the schema rather than moving it into application code.
- **Referral qualification is checked automatically whenever a credit lands.** Both sides need a
  verified social platform and 50 SEQ each earned from tasks, competitions or security reports;
  referral and pre-sale credits do not count toward that, and a referrer is capped at 20.
- **A verified platform is required to receive the launch payout and to earn referral rewards, but
  not to earn at all.** Blocking earning would cost onboarding; gating the payout is what actually
  matters.
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

<!-- BEGIN SHARED AGENT CONVENTIONS: identical in every Sequentia repo. Change it in all of them together. -->
## Working with git and GitHub here

These rules are the same in every Sequentia repository. They are repeated in each
one because this file is the only thing an agent is guaranteed to read, whatever
machine it is working from.

**Nothing pushed to GitHub credits Claude, Anthropic, or any AI tool.** No
`Co-Authored-By: Claude` trailer, no `Claude-Session:` trailer or `claude.ai`
link, no "Generated with Claude Code" in a commit message or a pull request body,
no `claude/*` branch names or session ids, and no mention in source, comments,
docs or issue text. Agent tooling offers several of these by default; compose the
message without them rather than stripping them afterwards.

**Author every commit as**
`GracedEternalKingCabbageMan <151803062+GracedEternalKingCabbageMan@users.noreply.github.com>`.
Never a personal address.

**Every change lands through a pull request that you merge yourself, at once.**
There is no reviewer on this project; the pull request exists so the reasoning is
recorded beside the diff. Branch, push, open it, merge it, delete the branch, all
in one sitting. Pushing straight to the default branch is the rule most often
broken here, and it is the one that costs the record. A pull request stays open
only when the repository owner asks for that specific one, and that never carries
over to the next.

**Name branches `area/short-description`**: `fix/`, `doc/`, `feature/`, `test/`,
`build/`, or the component being changed. Never a tool name, a session id, or
`worktree-*`.

**Write the subject as `area: what changed`**, one line, 72 characters at the
outside and 50 where you can manage it. Put the reasoning in the body, and
explain why rather than what.

**These repositories are public and world-readable.** Never commit private keys,
seeds, `wallet.dat`, RPC credentials, `.env` files or API tokens. Read the diff
before every commit. Secrets belong on the server and in offline backups.

**A file belongs to the repository whose code it describes.** Decide which repo
owns it before writing it; if it landed in the wrong one, move it rather than
deleting it.

**Push the same day you commit.** The testnet server pulls only from GitHub, so a
branch left on one laptop is invisible to every other machine and to the box.
<!-- END SHARED AGENT CONVENTIONS -->
