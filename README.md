# Emissio

Emissio is the Sequentia community rewards platform: an account-based web app where contributors earn the Sequence token (SEQ) for testnet work, competition wins, and accepted security reports. Balances are credited to an on-platform ledger and paid out at mainnet launch to a mainnet address the user registers; there is no mainnet today, and everything you interact with runs on the Sequentia public testnet.

Live instance: https://sequentiatestnet.com/emissio/

It is a single Go binary with embedded templates and SQLite storage (pure Go driver, no CGO).

## Where this fits in Sequentia

Sequentia is a Bitcoin sidechain for asset tokenization and disintermediated exchanges, built as a fork of Blockstream Elements 23.3.3. Emissio is a supporting service, not part of the protocol: it reads the chain through the block explorer's REST API to check task evidence, reads the public node's peer list, and holds one small testnet wallet of its own to pay testers in the tasks where it is the counterparty.

| Repo | One-liner |
|---|---|
| [`Sequentia`](https://github.com/ConcatenaLabs/Sequentia) | The Sequentia node (`sequentiad`, a fork of Elements 23.3.3): consensus, anchoring, proof of stake, open fee market, plus the canonical protocol documentation in `doc/sequentia/`. |
| [`sequentia-electrs`](https://github.com/ConcatenaLabs/sequentia-electrs) | The electrs fork: Rust indexer + Esplora REST API for Sequentia and its Bitcoin testnet4 parent chain. |
| [`emissio`](https://github.com/ConcatenaLabs/emissio) | Emissio: community rewards platform, earning Sequence tokens (SEQ) for testnet contributions. |

Protocol documentation lives in [`Sequentia/doc/sequentia/`](https://github.com/ConcatenaLabs/Sequentia/tree/HEAD/doc/sequentia).

## What it does

Accounts, the task catalog with automatic checks of the evidence, a tester dashboard, bug reports (from a form, or sent from inside a wallet), competitions, private security reports, social-account verification (Telegram, X, Reddit), referrals, mainnet payout-address registration, and the admin review desk.

One thing it deliberately does not do, which a user or an operator needs to know: there is no pre-sale purchase flow. The rules page describes pre-sale windows and the ledger reserves a `presale` kind, but no sale mechanics exist here.

This is testnet software supporting a testnet program. Reward balances are program commitments recorded in a database, not on-chain funds.

## For users: earning rewards

You need an account (email + password, 10 characters minimum). When the instance sends mail, a link confirms the address, and an account earns (submits evidence, files bug reports, requests a verification, enters a competition) once it has; a forgotten password is reset from a link sent to the address. Without mail, an admin makes a one-time reset link from the user list and sends it to the account's address by hand. Every account gets a public claim code, which ties evidence to your account: you put it in a social-media bio or an X post for verification, and it doubles as your referral code.

All amounts below are whole SEQ, credited to your ledger. On the testnet you work with tSEQ and testnet assets; the rewards themselves are Sequence tokens paid at mainnet launch. Launch reference price: 0.175 USD per SEQ. Program pool: 32,285,714 SEQ.

### Testnet tasks

Each task pays a fixed reward, most have a first-come cap, and you can have one submission per task (you may resubmit after a rejection). A txid can only ever be evidence for one account. Every task belongs to one surface of the public testnet (Sequentia Core, the block explorer, the faucet, Compages, the staking pools, Emissio), which is how the dashboard counts a tester's progress.

Each task runs a check on its evidence (`evidenceCheck` in `checks.go`; a task's check kind, which admins pick in the task editor, says which). The check answers one of three ways:

- **pass**: the evidence proves the task, bound to the account. The submission is approved and credited at once.
- **fail**: it does not, for a reason the tester can fix: no proof decimals, not yet confirmed, the fee in the wrong asset. The submission is refused with that reason and nothing is stored. A tester sure the check is wrong ticks a box and sends it to a reviewer instead.
- **review**: nothing automatic can decide, because a service is unreachable or the task has no check. The submission waits in the review queue.

An account an admin holds never has anything approved automatically: its passing evidence waits for a reviewer too.

### Proofs

A txid on its own proves that a transaction exists, not that the submitter made it, so every account has **proof decimals**: eight digits derived from its claim code (`proofTag` in `proof.go`), shown on the account page and substituted into task instructions. A transaction counts as evidence only if one of its transparent outputs carries an amount ending in exactly those decimals, for example `1.86718345` tSEQ. Any wallet can send an exact amount, and a stranger's transaction matches by accident with probability one in a hundred million per output.

On top of that, each task checks the property it is about, from the explorer's view of the transaction and, where the task is about a service, from that service's own public data:

- the fee output's asset, outputs in two assets, a blinded output, a new issuance, a reissuance or a burn of an asset whose issuance the same account submitted, the Bitcoin anchor of the containing block against a hash in the notes;
- the faucet: the transaction spends coins from two payments the faucet made, known by an input from one of the faucet's addresses (`EMISSIO_FAUCET_ADDRESSES`);
- staking: the proof output is a staking script (read from its bytes as the node reads it, every form: height or time lock, a committee BLS registration, a vesting lock) of at least 40,000 tSEQ; a delegation record names that stake's key as controller and a pool on the pool board's feed (`EMISSIO_POOLS`) as signer; a re-delegation spends that record into one for another pool; an unbonding spends the stake into an unbonding output of the same key;
- Compages (`EMISSIO_COMPAGES`): bridging in, the bridge's record of the deposit named in the notes (an Ethereum transaction hash or a Solana deposit address) names its delivery, which the tester's transaction spends; bridging out, the bridge's record of the redemption address shows the tester's transaction released;
- the release: the checksum in the notes matches a file in the download page's `SHA256SUMS` (`EMISSIO_SUMS`), and the public node sees a peer announcing the account code that runs that release.

The verdict is written into the submission's check note; an admin can re-run it from the queue.

Two tasks are proven by the node rather than by a transaction. With `EMISSIO_CLI` and `EMISSIO_DATADIR` set, the app reads the public node's peer list: **build from source** passes when a connected peer announces the client name `Emissio-<code>`, which only a rebuilt binary can carry; **run a node for a week** is judged from sightings the app records every ten minutes of peers whose user agent contains `emissio-<code>` (a `-uacomment`), and passes when a week's span shows the node in at least 80% of the slots.

### Tasks with Emissio as the counterparty

Emissio holds a testnet wallet of its own (`EMISSIO_WALLET`, a wallet of the public node). On two tasks it pays the tester, from the task page, an amount nobody can predict: a random fraction between 0.1 and 1 of a random asset the wallet holds, every held asset standing equal, with the payment's fee in that same asset. The tester then proves the payment reached a wallet they control:

- **Return a payment**: a transaction that spends the very output Emissio paid, pays exactly that amount of that asset back to Emissio's address, and pays its fee in that asset.
- **Unblind a confidential payment**: the payment goes to the tester's confidential address, so only the holder of its blinding key can see the amount; the tester names it in the notes, as a number or inside an explorer link that unblinds it. Five wrong amounts end that payment.

Emissio pays only accounts that have completed the first-transaction task, at most once a day and three times per task for one account, three times a day for one network address, and `EMISSIO_WALLET_DAILY_CAP` times a day in all. The wallet is always the one named: a name that reads as a reserve, treasury, bridge, faucet or service wallet is refused, so a misconfiguration cannot spend from one of those. The wallet is loaded on the node if a restart dropped it.

Task instructions carry the user's own values where `{PROOF}` (proof decimals) and `{CODE}` (account code) appear in the seeded text.

The same binding applies outside tasks. A **competition entry** must carry the account code on the work or its page; the app looks for it in the link and in the linked page's text, notes what it found for the judges, and refuses a link another account already entered. A **security report** must start with the account code, inside the encrypted block when encrypted, so a report read long after it was sent still names its account; identical bodies from two accounts are flagged.

The seeded catalog (`seed.go`; the live instance's admins can adjust rewards, caps, active flags and checks, and add tasks, from the task editor):

| Task | Surface | Reward | Cap |
|---|---|---|---|
| Make your first testnet transaction | Sequentia Core | 10 | 5,000 |
| Find the Bitcoin anchor of your transaction | Block explorer | 10 | 3,000 |
| Combine two faucet payments in one transaction | Faucet | 15 | 2,000 |
| Issue your own asset | Sequentia Core | 20 | 2,500 |
| Reissue an asset you created | Sequentia Core | 20 | 1,000 |
| Burn part of an asset you issued | Sequentia Core | 15 | 1,000 |
| Pay a fee in an asset other than tSEQ | Sequentia Core | 15 | 2,500 |
| Send two assets in one transaction | Sequentia Core | 15 | 2,000 |
| Send an opt-in confidential transaction | Sequentia Core | 15 | 2,000 |
| Return a payment to Emissio | Emissio | 25 | 1,000 |
| Receive a confidential payment and unblind it | Block explorer | 25 | 1,000 |
| Bridge an asset into Sequentia with Compages | Compages | 30 | 1,000 |
| Bridge from the other source chain | Compages | 30 | 500 |
| Bridge an asset back out with Compages | Compages | 40 | 500 |
| Install the release and check its checksum | Sequentia Core | 15 | 2,000 |
| Build Sequentia Core from source | Sequentia Core | 30 | 500 |
| Run a full node for a week | Sequentia Core | 50 | 1,000 |
| Stake tSEQ | Sequentia Core | 40 | 500 |
| Delegate your stake to a staking pool | Staking pools | 30 | 500 |
| Move your delegation to another pool | Staking pools | 20 | 500 |
| Unbond your stake | Sequentia Core | 25 | 500 |

Completing every task pays 495 SEQ; total task exposure across all users is 560,000 SEQ. Caps pay the first accounts whose evidence is approved, so the pool stays bounded.

### The dashboard

The account page opens with the tester's progress: for each surface, how many of its tasks are done and how many wait for review; the tasks still open, with their rewards; what was earned from what (tasks, bug reports, security reports, competitions, verified accounts, referrals, adjustments); and every bug and security report filed, with its status.

### Bug reports

The bug page (`/bugs`) takes a report of anything wrong on the public testnet: the surface (explorer, faucet, Compages, Sequentia Core, the staking pool board, this site), a title, the steps to reproduce, what was expected and what happened, and an optional screenshot (PNG, JPEG, GIF or WebP, up to 2 MB, judged by its bytes). An account files up to ten a day and follows each one's status on the same page. A report earns nothing until an admin reproduces and accepts it, at a severity whose reward is fixed in `bugTiers` (`bugs.go`):

| Severity | Reward |
|---|---|
| Minor: wrong text, a confusing screen, a broken link | 10 |
| Major: a feature that does not work as documented, a wrong number shown | 25 |
| Severe: lost or stuck testnet funds, a reproducible crash, a flow that cannot be completed | 50 |

The first report of a bug is the one rewarded. An admin marks a later one a duplicate of it, which merges it into the original (a duplicate of a duplicate is merged into the original too), and a report whose text is identical to another's is flagged on its page. Anything with a security consequence belongs on the security page.

Wallets file reports through the JSON intake described under "For integrators"; they land in the same queue and pay the same way, to the account whose code the person gave.

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

### The payout

Balances are paid in one payment from the paying wallet, straight to each account's wallet: a payout run. Each account's allocation is a share of its own that its wallet collects by itself the next time it opens; what is not collected by the run's deadline (a few weeks after the payment) goes back to the paying wallet, and the run records each account as collected or returned.

Each account is named by a public key its wallet holds:

- **The launch payout**, when it is paid this way, names the key of the registered mainnet address. A taproot address (`bc1p…`) is its key. An address starting `bc1q` names only a hash of its key, so with `EMISSIO_PAYOUT_LAUNCH_KEY=1` the account page asks for its public key once (66 characters, `02…` or `03…`) and checks it against the address; off (the default), the page asks for nothing, as an on-chain payment to the address needs no key. A script address (a `bc1q` of 62 characters) cannot be paid this way.
- **The testnet rehearsal** names the key of the first receive address of the account's Sequentia testnet wallet. The account page asks for it only while an admin has the rehearsal asking for wallets (`/admin/payouts`), and keeps showing it to an account that gave one or was paid; to anyone else the page shows nothing of the rehearsal. The account pastes its wallet's account key, which the web wallet shows under Receive, Show xpub (a `tpub`, with or without its `[fingerprint/84h/1h/0h]` origin, or the descriptor lines): Emissio derives the key of `m/84'/1'/0'/0/0` from it and keeps only that key. An address of the wallet that has already sent a transaction works too: its key is read from that spend through the explorer (`EMISSIO_ESPLORA`). A public key or a taproot address is taken as it is. A rehearsal pays tSEQ and moves nothing in any ledger.

A run takes every account with a positive balance, at least one verified platform and a key, at its balance times `EMISSIO_PAYOUT_ATOMS_PER_SEQ` atoms of `EMISSIO_PAYOUT_ASSET`; every other account is listed as left out with the reason. A wallet key is paid for one account only, and an account is in one open run at a time. On mainnet a collected allocation is a `payout` ledger row that takes it off the balance, once; one that comes back stays owed and goes into the next run.

The admin's side is `/admin/payouts`:

1. **For the rehearsal, ask for wallets first** ("Ask accounts for their testnet wallet"), and stop asking when the run is paid.
2. **Open a run.** The page shows how many accounts are paid, the total, the most the fee can be, and who is left out.
3. **Pay it.** Download the payment list (a line `KEY ATOMS` each) and pay it from the paying wallet, whose key never touches this host. With the `arca` command line: `arca distribute LIST --asset ASSET --quote` shows the exact fee; `arca distribute LIST --asset ASSET --shown FEE` pays; `arca sync` once its round is final.
4. **Record its reference.** Paste the payment's reference (the distribution id the wallet answers). It is recorded only when the payment service's public status names exactly the run's list: the asset, every key once, every amount, nobody else.
5. **Watch it.** The follower reads the status every `EMISSIO_PAYOUT_POLL_SECS` and records each allocation as paid, collected or returned; the account page tells the account in plain words.

The payment service is an Arca operator (`EMISSIO_PAYOUT_URL`), and the payment is a distribution in its claim-tree sense. Emissio only reads its public `/v1/info` and `/v1/distribution_status`; no page names it.

The launch payout needs, beyond this code: the Sequentia mainnet and an operator on it paying SEQ; for a `bc1p` key from a BIP86 wallet, a wallet library that looks for payments under BIP86 output keys (it looks under BIP84 keys and the identity key); and the public keys of `bc1q` accounts collected before launch.

## The review model

A task whose evidence passes its automatic check is credited at once; everything else is reviewed by an admin: task evidence the check could not decide or the tester sent for review, bug and security reports, verifications, and competition entries, which admins judge. The verification checks (the Reddit token, the X post, the Telegram bot) annotate the queue. Admin accounts are ordinary accounts with an admin flag, created from the server command line; there is no self-service admin signup.

Anti-farming, mechanically enforced in the database:

- Approval credits the ledger inside the same transaction as the status change, and partial unique indexes on the ledger make double credits impossible (one credit per submission, entry, report, verification, and referral).
- One transaction id can be evidence for one account only.
- One social account can verify one Emissio account, ever.
- Registration IPs are recorded and surfaced in the admin user list with a shared-IP counter.
- Manual admin ledger adjustments are always visible to the affected user.

## For integrators

Emissio is a server-rendered HTML application: session cookies (HttpOnly, SameSite=Lax), a per-session CSRF token on every POST, and a strict same-origin Content-Security-Policy. Its machine-readable endpoints are the problem-report intake below and the admin-only exports.

### Problem-report intake

`POST /api/problem-reports` (on the live instance, `https://sequentiatestnet.com/emissio/api/problem-reports`) takes a problem report from inside a wallet. It is anonymous: no cookies, no CSRF token, and `Access-Control-Allow-Origin: *` on every answer, with `OPTIONS` answered for the preflight (`POST`, `Content-Type`, max age a day). The body is JSON, at most 32 KiB:

```json
{
  "v": 1,
  "wallet": "web-wallet",
  "version": "1.2.3",
  "mode": "default",
  "description": "what happened",
  "account_code": "0123456789",
  "errors": [{"at": "2026-01-01T12:00:00Z", "message": "..."}],
  "context": {"platform": "...", "user_agent": "...", "network": "testnet"},
  "attached": {"addresses": ["tb1..."]}
}
```

`wallet` is one of `web-wallet`, `ambra-browser`, `ambra-mobile`; `description` is required. `account_code` is optional and links the report to the account with that code; a code that matches no account is kept with the report, which stays unattributed. Longer fields are cut to their limits (description 4,000 characters, version 40, mode 20, 20 errors of 500 characters each, the newest kept, context values 300, 20 addresses of 120) and unknown fields are ignored. Keys and seeds are never part of a report.

| Answer | Body | When |
|---|---|---|
| `201` | `{"id": 123, "account": "linked"}` (or `"unknown"`, `"none"`) | stored |
| `400` | `{"error": "Describe what happened."}` | not JSON, `v` other than 1, an unknown wallet, no description |
| `409` | `{"error": "This report was already received."}` | the same description from the same address within the hour |
| `413` | `{"error": "This report is too large."}` | over 32 KiB |
| `415` | `{"error": "Send the report as JSON."}` | another content type |
| `429` | `{"error": "Too many reports from here. Try again later."}`, with `Retry-After` | over 5 an hour or 20 a day from one address, or 10 a day for one account code |

A report is credited only when an admin accepts it, by severity, as for the bug page.

Public routes, for orientation (see `routes()` in `main.go` for the full list including the `/admin/*` desk):

| Route | Purpose |
|---|---|
| `GET /` | Home: program overview and stats |
| `GET /tasks`, `GET /tasks/{slug}`, `POST /tasks/{slug}/submit` | Task catalog and evidence submission |
| `POST /tasks/{slug}/request` | Asking Emissio's wallet for the payment of a counterparty task |
| `GET /competitions`, `GET /competitions/{slug}`, `POST .../enter` | Competitions and entries |
| `GET /bugs`, `POST /bugs`, `GET /bugs/{id}/screenshot` | Bug reports and their screenshots (to the reporter and admins only) |
| `POST /api/problem-reports` | Problem reports from wallets (see above) |
| `GET /security`, `POST /security/report` | Security program and private reports |
| `GET /guide`, `GET /rules` | Wallet/key-safety guide and program rules |
| `GET /account`, `POST /account/address`, `POST /account/verify` | Ledger, payout address, verifications |
| `GET /r/{code}` | Referral link (sets a 30-day cookie, redirects to registration) |
| `GET|POST /register`, `GET|POST /login`, `POST /logout` | Accounts (auth endpoints are rate-limited per IP) |
| `GET /verify-email`, `POST /account/verify-email` | Confirming the email address, sending the link again |
| `GET|POST /reset`, `GET|POST /reset/confirm` | Password reset by email |

Emissio reads these public APIs itself: an Esplora-compatible REST API (`/tx/<txid>`, `/asset/<id>`, `/block/<hash>`; `EMISSIO_ESPLORA`, publicly `https://sequentiatestnet.com/api`), Compages' (`/deposit/tx/<hash>`, `/sol/wrap/<address>`, `/redeem/<address>`, `/sol/redeem/<address>`; `EMISSIO_COMPAGES`, publicly `https://sequentiatestnet.com/bridge/api`), the pool board's feed (`EMISSIO_POOLS`, publicly `https://sequentiatestnet.com/pools/pools.json`) and the download page's `SHA256SUMS` (`EMISSIO_SUMS`, publicly `https://sequentiatestnet.com/download/SHA256SUMS`).

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

Running `createadmin` for an existing email resets that account's password and makes it an admin; a new admin's address counts as confirmed. The other subcommand, `./emissio reseed-tasks`, refreshes the title, category, and body of the seeded tasks from the current `seed.go` copy in an existing database, inserts any seeded task the database does not have yet, and deactivates the tasks listed as retired in `seed.go`, leaving admin-tuned rewards, caps, and active flags of the other existing tasks alone.

Tests:

```
go test ./...
```

`app_test.go` drives the full HTTP surface against a temporary database: registration, submission and review lifecycle, prize and report awards, referral qualification, verifications (with stubbed Reddit/Telegram servers), duplicate-txid rejection, base-path handling, CSRF, and anonymous-access denial. `bugs_test.go` covers the bug page (refusals, the screenshot's privacy, acceptance by severity paying once, duplicate merging) and the wallet intake (every answer of its contract, the limits, attribution by account code). `checks_test.go` covers every check's verdicts against a stand-in explorer and stand-in services (the staking-script parsers against scripts the node itself built, in every form), the release check against a stand-in peer list, the submission form's three outcomes and the dashboard. `regtest_tasks_test.go` runs the counterparty tasks against a real node on a private chain: Emissio's wallet pays, a tester's wallet returns or unblinds, and the checks read the transactions back, the wrong-coin and wrong-fee-asset returns and a wrong amount refused; it is skipped unless `EMISSIO_REGTEST_TASKS_CLI` and `EMISSIO_REGTEST_TASKS_DATADIR` name the node (the test's header says what it must hold). `mail_test.go` covers confirming the address (the gate, resend limits, only the newest link working), password reset (the same answer for unknown addresses, a link that works once, sessions ended), admin reset links, and the SMTP exchange itself against a stand-in server, credentials only after STARTTLS. `bech32_test.go` covers address validation and formatting helpers. `walletkey_test.go` covers reading a wallet's key from its account key (BIP84's own vector) and from an address that has spent, and that the rehearsal shows only when asked. `payout_test.go` covers payout keys, a rehearsal run against a stand-in payment service (left-out reasons, every refusal of a reference, collected and returned allocations, no balance moved) and the launch's single debit. `regtest_payout_test.go` runs the rehearsal end to end against a real payment service: an Arca operator from arca's `arca_operator_for_browsers` harness, named by `EMISSIO_REGTEST_CONTROL`, with `EMISSIO_REGTEST_ARCA` the `arca` binary; it is skipped without them.

### Configuration

All configuration is environment variables (`loadConfig` in `main.go`):

| Variable | Default | Meaning |
| --- | --- | --- |
| `EMISSIO_LISTEN` | `127.0.0.1:8095` | Listen address |
| `EMISSIO_DB` | `emissio.db` | SQLite database path |
| `EMISSIO_BASEPATH` | empty | Path prefix when served under one, e.g. `/emissio` |
| `EMISSIO_ESPLORA` | empty | Esplora API base URL for the transaction checks, e.g. `https://sequentiatestnet.com/api` (empty disables them; submissions then go to manual review) |
| `EMISSIO_POLICY_ASSET` | the testnet's tSEQ id | Asset id of tSEQ, for the fee and stake checks |
| `EMISSIO_CLI`, `EMISSIO_DATADIR` | empty | `sequentia-cli` path and the public node's data directory; enables the peer-based checks, the ten-minute sighting poller and Emissio's wallet |
| `EMISSIO_COMPAGES` | empty | Compages' API base, for the bridge checks (empty sends bridge evidence to review) |
| `EMISSIO_POOLS` | empty | The pool board's feed, for the delegation checks (empty sends them to review) |
| `EMISSIO_SUMS` | empty | The download page's checksum list, for the release check |
| `EMISSIO_FAUCET_ADDRESSES` | the testnet faucet's covenant address | Comma-separated addresses the faucet pays from, for the faucet check |
| `EMISSIO_WALLET` | empty | Name of Emissio's own wallet on that node, which pays the counterparty tasks (empty turns them off) |
| `EMISSIO_WALLET_ASSETS` | empty | Comma-separated asset ids the wallet may pay in; empty: any it holds |
| `EMISSIO_WALLET_DAILY_CAP` | `200` | Payments the wallet makes in a day, in all |
| `EMISSIO_SECURE` | `0` | Set `1` behind HTTPS so cookies are marked Secure |
| `EMISSIO_SMTP_HOST`, `EMISSIO_SMTP_PORT` | empty, `587` | SMTP submission service for verification and reset mail; with a sender set, mail is on. Port 587 uses STARTTLS (required before credentials are sent), 465 implicit TLS |
| `EMISSIO_SMTP_USER`, `EMISSIO_SMTP_PASS` | empty | Its credentials; keep the password in a systemd drop-in, never in the repo |
| `EMISSIO_MAIL_FROM` | empty | Sender, e.g. `Sequentia Emissio <emissio@sequentiatestnet.com>` |
| `EMISSIO_PUBLIC_URL` | derived from the request | Public base address used in mailed links, e.g. `https://sequentiatestnet.com/emissio` |
| `EMISSIO_TG_BOT_TOKEN` | empty | Telegram bot token (placeholder: `123456:ABC-...`); enables automatic Telegram ownership + ID-based age checks |
| `EMISSIO_TG_BOT_NAME` | empty | The bot's username (without `@`), shown in user instructions |
| `EMISSIO_REDDIT_TOKEN_SECRET` | empty | Secret shared with the Reddit app on r/sequentia that signs ownership tokens; enables the token flow for Reddit |
| `EMISSIO_REDDIT_CLIENT_ID`, `EMISSIO_REDDIT_CLIENT_SECRET` | empty | Credentials of a Reddit app (created at reddit.com/prefs/apps, type "script"); enables the automatic Reddit check. Reddit blocks unauthenticated requests, so without them Reddit requests go to manual review |
| `EMISSIO_REDDIT` | `https://www.reddit.com` | Reddit base URL, where tokens are issued (overridden in tests) |
| `EMISSIO_REDDIT_OAUTH` | `https://oauth.reddit.com` | Reddit API host, where profiles are read (overridden in tests) |
| `EMISSIO_X` | `https://cdn.syndication.twimg.com` | X embedded-post endpoint (overridden in tests) |
| `EMISSIO_TELEGRAM` | `https://t.me` | t.me base URL (overridden in tests) |
| `EMISSIO_PAYOUT_URL` | empty | Base URL (no `/v1`) of the payment service the payout runs pay through, an Arca operator serving the payout asset; empty turns payout runs off |
| `EMISSIO_PAYOUT_NETWORK` | `testnet` | `testnet`: the rehearsal, tSEQ to each account's testnet wallet key, no balance moved. `mainnet`: SEQ to the key of each account's payout address, a collected allocation debited |
| `EMISSIO_PAYOUT_ASSET` | `EMISSIO_POLICY_ASSET` on the testnet; none on mainnet | Asset id the payout pays in; required on mainnet |
| `EMISSIO_PAYOUT_ATOMS_PER_SEQ` | `100000000` | Atoms of the payout asset paid for each SEQ of balance (a rehearsal may pay less) |
| `EMISSIO_PAYOUT_HRPS` | `tb` | Address prefixes a testnet wallet key may be given in as a taproot address, comma-separated |
| `EMISSIO_PAYOUT_POLL_SECS` | `60` | How often the follower reads the status of a run with a payment |
| `EMISSIO_PAYOUT_LAUNCH_KEY` | `0` | `1`: the account page asks a `bc1q` account for its address's public key, which a launch payout paid into wallets needs and an on-chain payment does not |

### Sending mail

Mail goes out through an SMTP submission service: any transactional provider (Amazon SES, Postmark, Brevo, Mailgun and the like) works. For mail from `sequentiatestnet.com` to be delivered rather than filed as spam, the domain's DNS needs the provider's records: its SPF include merged into the existing SPF record (one `v=spf1` record per domain, so `v=spf1 include:spf.efwd.registrar-servers.com include:<provider> ~all`), the DKIM records the provider issues for the domain, and a DMARC record (`_dmarc` TXT, `v=DMARC1; p=none` to start). On the host, the SMTP password lives in a systemd drop-in readable by root alone, next to `EMISSIO_SMTP_HOST`, `EMISSIO_SMTP_USER`, `EMISSIO_MAIL_FROM` and `EMISSIO_PUBLIC_URL`, and the host must reach the provider on port 587. One account gets at most three mails an hour, one network address causes at most ten, and a link works once: a day for confirming the address, an hour for a reset (a day for a link an admin makes).

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
| `checks.go` | The task checks and their verdicts: staking scripts read from their bytes, the faucet, Compages, the release |
| `counterparty.go` | Emissio's own wallet: paying a tester, the return and unblind checks |
| `dashboard.go` | The tester's dashboard on the account page |
| `signals.go` | Addresses each account's evidence touched, for the admin's clustering signals |
| `bugs.go` | Bug reports: the form, screenshots, the wallet intake, the admin review |
| `mail.go` | Mail: the SMTP sender, confirming the address, password reset, admin reset links |
| `payout.go` | Payout runs: keys, the run's list, the reference check, the follower, the admin's and the account's pages |
| `walletkey.go` | Reading what an account pastes (an account key, an address that has spent, a key) into the key the rehearsal pays |
| `templates/`, `static/` | HTML templates and assets, embedded into the binary |
| `deploy/` | systemd unit and box install script |

### Data storage

One SQLite file (WAL mode, foreign keys on, a single connection to serialize writers). Tables: `users`, `sessions`, `tasks`, `submissions`, `competitions`, `entries`, `reports`, `ledger`, `verifications`, and for the payout `payout_keys`, `payout_runs`, `payout_rows`, `payout_settings`, for bug reports `bugs` and `bug_files` (screenshots), `email_tokens` for the hashes of mailed links, `emissio_payments` and `kv` for Emissio's wallet (its payments, its return address), and `chain_links` for the addresses each account's evidence touched. Every reward is a ledger row; a balance is `SUM(amount)` over the user's rows. Ledger kinds: `submission`, `entry`, `report`, `bug`, `verification`, `referral`, `referral-welcome`, `adjustment`, `payout` (a collected launch allocation, negative), and the reserved `presale`. Partial unique indexes on `(kind, ref_id)` are the double-credit guard.

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
