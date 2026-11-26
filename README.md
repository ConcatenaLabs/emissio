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

A front page for a visitor who has never heard of Sequentia, accounts, the task catalog with automatic checks of the evidence, a tester dashboard, bug reports (from a form, or sent from inside a wallet), competitions, private security reports, social-account verification (Telegram, X, Reddit), referrals, mainnet payout-address registration, and the admin review desk.

One thing it deliberately does not do, which a user or an operator needs to know: there is no pre-sale purchase flow. The rules page describes pre-sale windows and the ledger reserves a `presale` kind, but no sale mechanics exist here.

This is testnet software supporting a testnet program. Reward balances are program commitments recorded in a database, not on-chain funds.

## For users: earning rewards

You need an account (email + password, 10 characters minimum), one per mailbox: an address with a +tag, or a Gmail address with dots, delivers to the same inbox as the plain one, so registration refuses it once that mailbox has an account (`emailkey.go`); accounts that already share a mailbox are linked on the admin desk. When the instance sends mail, a link confirms the address, and an account earns (submits evidence, files bug reports, requests a verification, enters a competition) once it has; a forgotten password is reset from a link sent to the address. Without mail, an admin makes a one-time reset link from the user list and sends it to the account's address by hand. Every account gets a public claim code, which ties evidence to your account: you put it in a social-media bio or an X post for verification, and it doubles as your referral code.

All amounts below are whole SEQ, credited to your ledger. On the testnet you work with tSEQ and testnet assets; the rewards themselves are Sequence tokens paid at mainnet launch. Launch reference price: 0.175 USD per SEQ. Program pool: 32,285,714 SEQ.

### Testnet tasks

Each task pays a fixed reward, most have a first-come cap, and you can have one submission per task (you may resubmit after a rejection). A txid can only ever be evidence for one account, also when two accounts submit it at the same moment: a submission's claim queries, its check and its insert run as one step for that txid and that account's task. Every task belongs to one stage of a single path (web wallet, staking, Sequentia Core, assets, fees, bridge, infrastructure), named by its category, and is done on one surface of the public testnet (Sequentia Core, the block explorer, the faucet, Compages, the staking pools, the web wallet), which its page shows as a label. The stages are how the tasks are grouped and how the dashboard counts a tester's progress; see [The order of the tasks](#the-order-of-the-tasks).

Each task runs a check on its evidence (`evidenceCheck` in `checks.go`; a task's check kind, which admins pick in the task editor, says which). The check answers one of four ways:

- **pass**: the evidence proves the task, bound to the account. The submission is approved and credited at once.
- **fail**: it does not, for a reason the tester can fix: no proof decimals, the fee in the wrong asset, a node announcing the code the wrong way. The submission is refused with that reason and nothing is stored; where the transaction shows the likely slip (the fee taken out of the amount, the faucet's payment submitted instead of the tester's own send, the anchor's height instead of its hash, a delegation's authorization transaction instead of its record) the reason names it, and the refused txid goes back into the form. A failure never goes to a reviewer; a tester sure the check misreads the evidence reports it on the bug page.
- **wait**: the evidence has not arrived yet and arrives without the tester doing more: a transaction not yet confirmed, a payment not yet at Emissio's receive code, a bridge delivery or release not yet made, a node not yet connected to the public node or not yet seen for a week. An unconfirmed transaction waits only once it carries the account's proof decimals, where the task asks for them; without them it is refused at once, so nobody can park another tester's transaction. The submission is stored as waiting (`waiting.go`); every three minutes Emissio runs the same check again and credits it the moment it passes, refuses it if the check now fails, sends it to the queue at once if the check now says a person must judge it, and after the task's waiting time (two weeks for the week-of-uptime task, three days for the rest) it lapses with the check's last note. A check that could not run (a service unreachable) keeps it waiting, and if it still cannot run when the time is up, the submission goes to a reviewer. A new submission for the task replaces a waiting one. A waiting submission is no claim on its transaction: it never keeps another account from submitting it, and if another account's submission of it is pending or approved by the time the check passes, the waiting one is refused.
- **review**: nothing automatic can decide, because a service is unreachable, the task has no check, or the evidence needs judgment (a second account from one Bitcoin node's address, two nodes of one tester on two addresses). The submission waits in the review queue.

Every automatic credit, at submission or from the waiting poller, every automatic verification and every referral payment goes through one boundary (`autocredit.go`) that reads two things inside the approval's own transaction: whether an admin holds the account, and whether it shares a network address, a browser or a chain address (including the addresses that funded the evidence being credited) with another account that no reviewer has cleared. The funding addresses come from the very transaction the check judged, never from a second read; a transaction check that hands over no transaction leaves the submission waiting rather than crediting on what is known. Either one sends the passing evidence to a reviewer, with the reason in its note, so a hold or a shared signal that appears while a check runs or while a submission waits still counts. A reviewer who judges that linked accounts are not one person farming (a household, an office) clears what they share on the account's admin page, with a reason. A clearance records each signal it covers: this network address, this browser, this chain address, between this pair of accounts, both ways (`cleared_signals`). Automatic credits stop weighing exactly those; any other value the same pair comes to share, and any value shared with another account, sends the next credit to a reviewer again.

### Proofs

A txid on its own proves that a transaction exists, not that the submitter made it, so every account has **proof decimals**: eight digits derived from its claim code (`proofTag` in `proof.go`), shown on the account page and substituted into task instructions. A transaction counts as evidence only if one of its transparent outputs carries an amount ending in exactly those decimals, for example `1.86718345` tSEQ. Any wallet can send an exact amount, and a stranger's transaction matches by accident with probability one in a hundred million per output.

On top of that, each task checks the property it is about, from the explorer's view of the transaction and, where the task is about a service, from that service's own public data:

- the fee output's asset, outputs in two assets, a blinded output, a new issuance, a reissuance or a burn of an asset whose issuance the same account submitted, the Bitcoin anchor of the containing block against a hash in the notes;
- the faucet: the transaction spends coins from two payments the faucet made, known by an input from one of the faucet's addresses (`EMISSIO_FAUCET_ADDRESSES`);
- staking: the proof output is a staking script (read from its bytes as the node reads it, every form: height or time lock, a committee BLS registration, a vesting lock) of at least 40,000 tSEQ; a delegation record names that stake's key as controller and a pool on the pool board's feed (`EMISSIO_POOLS`) as signer; a re-delegation spends that record into one for another pool; an unbonding spends the stake into an unbonding output of the same key;
- Compages (`EMISSIO_COMPAGES`): bridging in, the bridge's record of the deposit named in the notes (an Ethereum transaction hash or a Solana deposit address) names its delivery, which the tester's transaction spends; bridging out, the bridge's record of the redemption address shows the tester's transaction released;
- the release: the checksum in the notes matches a file in the download page's `SHA256SUMS` (`EMISSIO_SUMS`), and the public node sees a peer announcing the account code that runs that release;
- the fee list (`feetasks.go`): the fee is paid in USDX (`EMISSIO_USDX_ASSET`) and follows from the USDX value the account was given. A node pays `ceil(fee_rate × vsize × 1e8 / rate)` atoms of the fee asset, where the fee rate is in reference atoms per vbyte and the rate is that asset's on the sending node, so the fee of a transaction sent at the tasks' `fee_rate=2` shows the rate the tester's node held. Each account gets a USDX price from its proof decimals: 0.30 to 0.45 for the whitelist task (listed by hand with `setfeeexchangerates`), 0.55 to 0.70 for the price-server task (a manual price in the price server's config). The ranges do not overlap and both are below the network's 1, so a producer always values the fee at least as highly as the tester's node. The check allows the vsize a byte below or two above the explorer's, since a wallet prices the size it estimates before signing. A refusal says what the fee shows instead: USDX at the network's rate, the other task's price, or the rate the fee implies at that fee rate. A wallet that would leave change too small to keep drops the change output and adds it to the fee; a transaction with nothing but the proof output and the fee, and a fee above the rate's, is refused for that reason with what to do instead (send again from a larger coin, so the transaction keeps change), since its fee no longer shows the rate. The texts ask the tester to hold well more USDX than they send, so it rarely happens. The check sees the fee and nothing of the node, so a rate set by hand, the price server's push and a fee output written by hand look alike.

The verdict is written into the submission's check note; an admin can re-run it from the queue.

Two tasks are proven by the node rather than by a transaction. With `EMISSIO_CLI` and `EMISSIO_DATADIR` set, the app reads the public node's peer list: **build from source** passes when a connected peer announces the client name `Emissio-<code>`, which only a rebuilt binary can carry; **run a node for a week** is judged from sightings the app records every ten minutes of peers whose user agent contains `emissio-<code>` (a `-uacomment`), and passes when a week's span shows the node in at least 80% of the slots.

**Validate anchors with your own Bitcoin node** (`ownbitcoin.go`) puts together what the public side sees, since Emissio cannot look into a tester's node. It requires all of these:
- the box's Bitcoin node has a peer announcing `emissio-<code>`, and no other code, in its user agent. The node counts that peer as synced (its headers and blocks within three of the node's own height), it has been connected for ten minutes, and the same connection is in two successive peer lists. The list (address, user agent, direction, sync heights, services and connection time of each peer, the node's height, and the peers of the list before it) is written every minute by a timer on the box to the file `EMISSIO_BTC_PEERS`, so Emissio holds no Bitcoin node credential; a list older than five minutes sends the submission to a reviewer;
- the public Sequentia node sees a peer announcing the code from the same address. Two addresses go to a reviewer, with a message that says so;
- the shared Bitcoin endpoint, which a testnet node uses unless told otherwise, has had no call from that address for fifteen minutes. The endpoint keeps the time of each address's last call for a day and answers `EMISSIO_ANCHOR_SEEN` (its `/last-seen`) to the box only, with the time its record began and the last time it was full and left a caller out. An address the record holds is followed even while the record is full, so its silence is known. For an address it does not hold (one that never called the endpoint, or last did more than a day ago), a record younger than fifteen minutes tells the tester when to try again, and a record that was full within fifteen minutes sends the submission to a reviewer, since a call could have gone unseen. Whoever fills the record (it holds 100,000 addresses) can so keep those submissions with reviewers, and no others;
- the address has vouched for no other account (`btc_node_hosts`): a Bitcoin node's address vouches for one account, ever, and a second account from it goes to a reviewer.

A user agent is what a peer announces, and the check cannot see the nodes' settings or which Bitcoin node the tester's node really asks, only that it has stopped asking the shared one. A `getanchorstatus` pasted in the notes is not evidence: when the check refuses, a status that shows a problem (`validateanchor` false, `anchorstatus` other than `ok`) says what is wrong. The task is seeded switched off: an admin switches it on in the task editor once the peer list and the endpoint's record are live on the server.

### Tasks with Emissio as the counterparty

Emissio holds a testnet wallet of its own (`EMISSIO_WALLET`, a wallet of the public node). On two tasks it pays the tester, from the task page, an amount nobody can predict: a random fraction between 0.1 and 1 of a random asset the wallet holds, every held asset standing equal, with the payment's fee in that same asset. The tester then proves the payment reached a wallet they control:

- **Return a payment**: a transaction that spends the very output Emissio paid, pays exactly that amount of that asset back to Emissio's address, and pays its fee in that asset.
- **Unblind a confidential payment**: the payment goes to the tester's confidential address, so only the holder of its blinding key can see the amount; the tester names it in the notes, as a number or inside an explorer link that unblinds it. Five wrong amounts end that payment.

Emissio pays only accounts that have completed the first-transaction task, at most once a day and three times per task for one account, three times a day for one network address, and `EMISSIO_WALLET_DAILY_CAP` times a day in all. The wallet is always the one named: a name that reads as a reserve, treasury, bridge, faucet or service wallet is refused, so a misconfiguration cannot spend from one of those. The wallet is loaded on the node if a restart dropped it.

### Web wallet tasks

The web wallet's tasks are worded as its default mode speaks: receive codes, Send, Swap, BTC, an invoice.

- **Link your web wallet**: the account signs `Emissio <account code>` with the wallet and Emissio checks the signature. In one step, **Connect your web wallet** on the task page asks the web wallet open in another tab of the same browser to sign it (the provider protocol's `signMessage`, approved in the wallet's own dialog) and submits the address and signature; by hand, the account signs in the wallet's Sign tab and pastes the signature with its receive code. Either way the server trusts only the signature. The signing key is recovered from the signature (`walletlink.go`) and must be the key of the receive code's address; the wallet is then linked to the account, one account per wallet ever, and its key serves the payout rehearsal when the account gave none. The other web wallet tasks need the link.
- The rest have Emissio as the counterparty, through a wallet of its own on the testnet's Arca operator: the `arca` command line on a directory of its own (`EMISSIO_ARCA`, `EMISSIO_ARCA_DATADIR`), as the faucet runs its leaf wallet (`arcawallet.go`). Each account gets receive codes of its own from Emissio, `bitcoin:<address>?ark=<a Bitcoin ark address of its own>&sequentia=<a receive request of its own>`, so whatever reaches one is known to come from that account's wallet. A code an account pastes is read by `receivecode.go`, the same reader the faucet and Compages carry: in either case, its bech32 parts lower-cased, and a short Sequentia request (`SQR1…`, as the web wallet writes it) expanded with Emissio's own operator key and genesis hash, so a code for another operator or chain is refused. Every address field (a payment's address, the payout address, the wallet link) takes a receive code too and reads its address. A follower syncs the wallet on its own schedule (at least every five minutes) and reconciles the wallet's coins against the codes: whichever command accepted a coin (the follower's own sync, a payment's, a sync run by hand) and whenever, the coin is in the wallet's list of every coin it holds or held (`arca coins`), and each coin not read before has its record read and kept (`arca_coins`), with the code it pays: the code whose owner key and nonce are those of the coin's own leaf. The record is read field by field as the wallet reads it (`arcarecord.go`, both of arca's record formats), never searched: a transfer's record carries every coin of its history ahead of its own leaf, so a search would find a coin that once paid another code. A record that does not parse to its last byte is matched to nothing. A code takes the first coin that pays it; one coin pays one code; a later coin to a paid code is kept as not counted, and a coin of no code is logged once. The coin's read and its code's payment are one write. A record the wallet does not give, or that Emissio cannot read, is asked for again for as long as it takes, after a minute and then less often, up to every six hours, and the desk names each such coin meanwhile. A follow reads at most 50 records: those its own sync accepted first, then the ten oldest unread, then the newest, so a payment is read in the follow it arrives in and no stream of new coins keeps an older one waiting. The list of coins is the wallet's whole history, with work in the wallet for each coin, so while no code is open, no coin is unread and nothing arrives, it is read once an hour. Emissio never pays one of its own codes: a receive code pasted in the round trip, or given by a payment name, whose key is one of Emissio's own is refused, since the coin would land back in Emissio's wallet and credit the account. It reads what reached each account's Bitcoin ark address from Bark's history. Each of these task pages says what the account does next and what Emissio has not seen yet; the submission form opens once the task's check passes. Before that, an account sure it has done the step can submit it anyway: the submission waits, Emissio checks again every few minutes and credits it the moment the payment shows, for up to three days. Asking Emissio to pay again sits behind a warning, since only the newest payment counts. A receive code takes one payment, as the operator refuses a second one to its key, so a code can always be replaced: once it is paid and the check still fails (a swap paid without the proof decimals), and once it is ten minutes old and unpaid (a wallet that will not pay it), the page offers a new one, at most five a day per account and task, one request of an account's task at a time, so requests sent at once make no more than that, and Emissio pays an account no more than three times per task however many requests run together. A payment to any of the account's codes for the task counts, the newest being only the one the page shows: every code was made for that account alone, so a payment to an older one is the same proof, and a tester who paid and then asked for a new code keeps the credit. In the round trip and the payment name, a new code is for returning Emissio's newest payment, and a return to any code made since that payment counts. Beside a waiting, rejected or queued web wallet submission the review desk shows what the wallet holds for it: each of the account's codes for the task, the coin that paid it, any coin naming one that was not counted, and when the wallet's coins were last read, with how many records were still to read, and every coin whose record could not be read yet, by its leaf, so a reviewer never says nothing arrived while the wallet has not been read.
  - **Pay Emissio**: anything reaching the account's code.
  - **Receive and send back**: Emissio pays the account's receive code a random fraction of a random asset it holds, and never shows the amount; the account sends what arrived to Emissio's code. The return may fall up to 0.001 of a unit short (`returnShortAtoms`): a transfer out of round leaves margins for its transactions' fees out of the payer's own coins, so a wallet that holds only what Emissio paid can send back a little less than all of it.
  - **Pay in BTC**: BTC reaching the code's Bitcoin ark address.
  - **An invoice**: Emissio pays an invoice the account's wallet made (a receive code with an amount carries one), in BTC or in a Sequentia asset Emissio holds (`invoicelimit.go`). A BTC invoice may ask up to 10,000 sats; one in a Sequentia asset up to about $10 of it at the price feed's price, rounded down to a round amount of the asset (10 USDX, 5 EURX, 0.002 GOLD), so the limit reads as an amount a person would type. An asset the registry does not name or the feed does not price is not paid. The task page lists the limit in every asset, and a refusal names it.
  - **A swap**: a payment to the account's code ending in its proof decimals, in an asset other than tSEQ.
  - **A payment name**: the account gives the payment name its web wallet chose (`name@sequentiatestnet.com`). Emissio asks the name service (`EMISSIO_LNADDR`, the LSP's `/lnaddr`) for that name's receive code, which the service has only while the wallet is open (`fresh`), and pays it as in the round trip; the account sends what arrived to Emissio's code. The return may fall up to 0.001 of a unit short (`returnShortAtoms`): a transfer out of round leaves margins for its transactions' fees out of the payer's own coins, so a wallet that holds only what Emissio paid can send back a little less than all of it. Only the wallet holding the name knows the amount, so a name another person holds earns nothing.

  Emissio pays only linked accounts, three times per task for one account and three times a day for one network address. Every arca call is serialised: the wallet's store is not safe for two processes at once.

Task instructions carry the user's own values where `{PROOF}` (proof decimals) and `{CODE}` (account code) appear in the seeded text, and `{PRICE_A}`, `{RATE_A}`, `{PRICE_B}` and `{RATE_B}` (the account's USDX price and node rate for the whitelist and price-server tasks).

The same binding applies outside tasks. A **competition entry** must carry the account code on the work or its page; the app looks for it in the link and in the linked page's text, notes what it found for the judges, and refuses a link another account already entered. A **security report** must start with the account code, inside the encrypted block when encrypted, so a report read long after it was sent still names its account; identical bodies from two accounts are flagged.

The seeded catalog (`seed.go`; the live instance's admins can adjust rewards, caps, active flags and checks, and add tasks, from the task editor):

| Task | Stage | Done on | Builds on | Reward | Cap |
|---|---|---|---|---|---|
| Link your web wallet | Web wallet | Web wallet | | 10 | 5,000 |
| Pay Emissio from your web wallet | Web wallet | Web wallet | Link your web wallet | 15 | 3,000 |
| Receive from Emissio and send back what arrived | Web wallet | Web wallet | Link your web wallet | 25 | 2,000 |
| Swap, then pay Emissio in what you got | Web wallet | Web wallet | Link your web wallet | 25 | 2,000 |
| Get paid at your payment name | Web wallet | Web wallet | Link your web wallet | 20 | 1,000 |
| Get paid an invoice by Emissio | Web wallet | Web wallet | Link your web wallet | 20 | 1,000 |
| Pay Emissio in BTC | Web wallet | Web wallet | Link your web wallet | 20 | 2,000 |
| Stake tSEQ | Staking | Web wallet | | 40 | 500 |
| Delegate your stake to a staking pool | Staking | Staking pools | Stake tSEQ | 30 | 500 |
| Move your delegation to another pool | Staking | Staking pools | Delegate your stake to a staking pool | 20 | 500 |
| Unbond your stake | Staking | Sequentia Core | Stake tSEQ | 25 | 500 |
| Install the release and check its checksum | Sequentia Core | Sequentia Core | | 15 | 2,000 |
| Make your first testnet transaction | Sequentia Core | Sequentia Core | Install the release and check its checksum | 10 | 5,000 |
| Find the Bitcoin anchor of your transaction | Sequentia Core | Block explorer | Make your first testnet transaction | 10 | 3,000 |
| Combine two faucet payments in one transaction | Sequentia Core | Faucet | Make your first testnet transaction | 15 | 2,000 |
| Send two assets in one transaction | Sequentia Core | Sequentia Core | Make your first testnet transaction | 15 | 2,000 |
| Send an opt-in confidential transaction | Sequentia Core | Sequentia Core | Make your first testnet transaction | 15 | 2,000 |
| Receive a confidential payment and unblind it | Sequentia Core | Block explorer | Make your first testnet transaction | 25 | 1,000 |
| Issue your own asset | Assets | Sequentia Core | Make your first testnet transaction | 20 | 2,500 |
| Reissue an asset you created | Assets | Sequentia Core | Issue your own asset | 20 | 1,000 |
| Burn part of an asset you issued | Assets | Sequentia Core | Issue your own asset | 15 | 1,000 |
| List an asset for fees on your node | Fees | Sequentia Core | Make your first testnet transaction | 25 | 1,000 |
| Pay a fee in an asset other than tSEQ | Fees | Sequentia Core | List an asset for fees on your node | 15 | 2,500 |
| Return a payment to Emissio | Fees | Sequentia Core | List an asset for fees on your node, Make your first testnet transaction | 25 | 1,000 |
| Run the price server | Fees | Sequentia Core | List an asset for fees on your node | 30 | 500 |
| Bridge an asset into Sequentia with Compages | Bridge | Compages | Make your first testnet transaction | 30 | 1,000 |
| Bridge from the other source chain | Bridge | Compages | Bridge an asset into Sequentia with Compages | 30 | 500 |
| Bridge an asset back out with Compages | Bridge | Compages | Bridge an asset into Sequentia with Compages | 40 | 500 |
| Validate anchors with your own Bitcoin node (seeded switched off) | Infrastructure | Sequentia Core | Install the release and check its checksum | 40 | 500 |
| Run a full node for a week | Infrastructure | Sequentia Core | Install the release and check its checksum | 50 | 1,000 |
| Build Sequentia Core from source | Infrastructure | Sequentia Core | Install the release and check its checksum | 30 | 500 |

Completing every task pays 725 SEQ; total task exposure across all users is 895,000 SEQ. Caps pay the first accounts whose evidence is approved, so the pool stays bounded.

### The order of the tasks

The task page is one path, in the order a tester does things, and each stage goes from easiest to hardest: the web wallet, which needs no install; staking, from the web wallet; Sequentia Core, installed and verified first; assets; fees, where the node's own fee list comes before the tasks that pay a fee in another asset (a node pays a fee only in an asset on its own list); the bridge; and infrastructure. The stages are the list `stages` in `seed.go`, and a task's category names its stage. A task filed under any other category is listed last, under Other tasks.

Each seeded task names the tasks it builds on (`needs` in `seed.go`, shown in the table above), and its `sort` places it on the path. `TestSeededTasksFollowThePath` fails if a task comes before one it needs, names a task that does not exist, sits under no stage, or if `sort` does not rise along the path, and if a stage has no task.

### The front page

The front page is written for someone arriving from an announcement who has never heard of Sequentia. It says in one line what Sequentia is, then gives two steps side by side. The first, start testing, needs no account: open the web wallet (`EMISSIO_WALLET_URL`), make a wallet, and take test coins from its faucet card. The second invites the visitor to join and earn, and says what earning means: Sequence tokens (SEQ) credited for tasks, bug reports, competitions, vulnerability reports and referrals, paid at mainnet launch. Signed in, the second step shows the tester's balance, how many tasks are done, and the next open task. The web wallet's stage comes first, as tiles; every other stage is summed up (its count, its total reward and, signed in, how many are done), linking to that stage's section of the task page, which groups the tasks the same way. The next task, here and on the dashboard, is the first open one on the path. The page shows no counts of accounts or of tokens credited.

### Connecting a wallet

The link task's **Connect** button goes through the same connector layer as SeqDEX: `static/shared/connectors.js` and `static/shared/provider.js`, with the libraries they load, vendored from [`seqdex-web`](https://github.com/ConcatenaLabs/seqdex-web) unchanged under a header line naming the commit. `vendor_test.go` fails when a vendored file is edited here or differs from that commit upstream; to change one, change it in `seqdex-web` and vendor it again. The page tells `provider.js` which connectors to offer (`EMISSIO_CONNECTORS`), and whichever wallet answers only signs the link message.

### The dashboard

The account page opens with the tester's progress: for each stage of the path, how many of its tasks are done and how many wait for review; the tasks still open, with their rewards; what was earned from what (tasks, bug reports, security reports, competitions, verified accounts, referrals, adjustments); and every bug and security report filed, with its status.

### Bug reports

The bug page (`/bugs`) takes a report of anything wrong on the public testnet: the surface (explorer, faucet, Compages, Sequentia Core, the staking pool board, this site), a title, the steps to reproduce, what was expected and what happened, and an optional screenshot (PNG, JPEG, GIF or WebP, up to 2 MB, judged by its bytes). An account files up to ten a day and follows each one's status on the same page. A report earns nothing until an admin reproduces and accepts it, at a severity whose reward is fixed in `bugTiers` (`bugs.go`):

| Severity | Reward |
|---|---|
| Minor: wrong text, a confusing screen, a broken link | 10 |
| Major: a feature that does not work as documented, a wrong number shown | 25 |
| Severe: lost or stuck testnet funds, a reproducible crash, a flow that cannot be completed | 50 |

The first report of a bug is the one rewarded. An admin marks a later one a duplicate of it, which merges it into the original (a duplicate of a duplicate is merged into the original too), and a report whose text is identical to another's is flagged on its page. Anything with a security consequence belongs on the security page.

Wallets file reports through the JSON intake described under "For integrators"; they land in the same queue and pay the same way, to the account whose code the person gave.

A report our own automated checks file, to prove the path works end to end, is a test report: the intake's `"test": true`, or a description (intake) or title (form) that starts with `EMISSIO-TEST`. It is stored with the status `test`, never enters the review queue and earns nothing.

### Competitions

Judged contests with ranked prize ladders (the seeded artwork competition pays 2,000 / 750 / 250 SEQ). One entry per account, editable until the competition closes. Admins assign places; each award is a ledger credit. The admin page for a competition edits its slug, title, body, prizes and closing date; a competition whose date has passed refuses new entries until the date is moved.

### Security reports

Private vulnerability reports with severity tiers; only the reporter and admins can see a report. Accepting a report credits the award. Default tiers: Low 1,000, Medium 6,000, High 30,000, Critical 150,000 SEQ. Every tier requires a defect in shipped code, a demonstrated security consequence and a reproduction a reviewer can confirm; bugs without a security angle go through the "Report a bug" task. Reporters can encrypt a report to the team's PGP key, served at `static/security-pgp.asc` with its fingerprint in `seed.go`; the private key is held by the review team off the server, and the admin views mark encrypted reports so they are decrypted locally rather than read in the browser. An exceptional finding (network-wide consequences, a truly novel technique, a working proof of concept) can be awarded up to 1,500,000 SEQ; 16,000,000 SEQ of the pool is reserved for security awards. The tier amounts, the ceiling and the reserve are constants in `seed.go`.

### Account verification (non-KYC)

Linking at least one verified social platform is required to receive the launch payout and to earn referral rewards. Earning itself is not blocked, so you can start on tasks immediately.

You can link a Telegram, X, and Reddit account, 15 SEQ per platform. As sybil resistance, the linked account must be at least two years old, and a social account can vouch for exactly one Emissio account, ever. Ownership proof is your claim code:

- Reddit: with `EMISSIO_REDDIT_TOKEN_SECRET` set, the user gets a signed token from the Emissio app installed on r/sequentia (the `emissio-reddit-verifier` repository), which Reddit authenticates, and pastes it into the account page; the token carries the username, the account creation date and the account code, and is verified here with the shared secret. Without that secret, the user puts the code in their profile description; with Reddit API app credentials the description and age are checked through the API, otherwise a reviewer checks by hand, since Reddit blocks unauthenticated requests.
- X: publish any post from the account's own profile that contains the code (a post in the tester's own words, a reply or a quote: no particular text is required) and submit its link. The handle is taken from the link, so the account locked by the one-account-ever rule is the one that published the code. The app fetches the post from X's keyless embedded-post endpoint (`EMISSIO_X`, default `https://cdn.syndication.twimg.com`) and checks that the author matches that handle (X shows a post under any handle in the URL), that the text contains the code, and the account age, derived from the author's numeric id, which encodes the creation time for accounts made since 2016 and predates that for smaller ids. A post already attached to another request is flagged in the queue.
- Telegram: if the instance runs a verification bot, you send your code as a message to the bot, and that is all. One poller (`tgbot.go`) reads the bot's messages as they arrive, long-polling the Bot API and confirming each message by asking for the next, with the offset kept in the database so a restart or an outage picks up where it stopped (the Bot API keeps an unconfirmed message for 24 hours). It answers every message in its chat, in plain words. A message that carries an account code verifies that account's Telegram on the message alone, through the same email gate and the same automatic credit boundary as every other credit: the message proves ownership through the sender's authenticated numeric ID, which the verification records, and account age is estimated from the ID (Telegram does not publish creation dates). The sender's ID, username and text of each such message are kept for a day. A message files nothing a reviewer would have to judge: where the ID is beyond the newest anchor of the age table, so its age cannot be vouched for, or where the account is held or shares a signal no reviewer has cleared, the bot tells the sender that only the account's owner can ask, and the owner does so from the account page (the form reads the kept message). So a stranger's message either verifies an account at once or leaves no trace on it. Anyone can send any account's code: a stranger could verify an account with their own Telegram account, which gains the stranger nothing and costs them a Telegram account that can never vouch again. The answers say nothing of an account beyond what the sender's own message did: whether another Telegram account verified it, whether it waits for a reviewer, or whether its email is confirmed is never told; whether a code exists is, as codes are public in referral links. One Telegram account vouches for one Emissio account: its ID is unique among verified rows, one request of a Telegram ID is checked and filed at a time whichever account it names, and an ID with a live request files no other. The review desk shows whether the poller reads the Bot API, with its last error, and any guard of the schema the database could not take. The account page names the bot (`EMISSIO_TG_BOT_NAME`, or the name the Bot API gives for the token) and shows the verification once it exists. Without a bot, the app falls back to a public t.me bio check with reviewer-judged age.

A request whose check passes (the code where only the owner could put it, the account at least two years old) is approved and credited at once, unless the Emissio account shares a network address, a browser or a chain address with another account that no reviewer has cleared, or an admin holds it: then it waits for a reviewer, with the reason in its note. A request the check refuses (code not found, no message to the bot, an account under two years, someone else's post, a social account that already vouches for another Emissio account) is turned down at once with that reason and never queued. Only what the check cannot judge waits for a reviewer: a service unreachable, Telegram's age without the bot, an age estimate the ID cannot vouch for, an X post already attached to another request.

### Referrals

Your claim code is also a referral link: `https://sequentiatestnet.com/emissio/r/<code>`. A referral pays 10 SEQ to each side, but only after both accounts hold a verified social platform and have each earned at least 50 SEQ from tasks, competitions, security reports, or positive admin adjustments (referral and pre-sale credits do not count), and for at most 20 referrals per referrer. Qualification is checked automatically whenever a credit lands, and the payment is an automatic credit like any other: if either side is held nothing is paid, and if either side shares an uncleared signal with another account the referral waits in the review queue (`referral_reviews`), where a reviewer approves it (both sides are paid, once, within the cap) or rejects it (it is never paid). The item shows a hold on either side, and approving pays nothing while one lasts. Whether the referral is already paid or has a review row is read inside the payment's own transaction, so a check that began before a payment or a decision can neither pay nor queue it again; a review row that meets a referral already paid is closed as paid.

Signed in, the front page, the task page and the account page carry an invite kit (`share.go`): the account's code and referral link, with buttons that open a post on X, a text post on Reddit and a message on Telegram, or copy the link. On X the kit writes only the link and the code and asks the tester to write their own words around them: X limits posts that read alike across many accounts, so the kit offers no ready-made X text. The code stands outside the link, since X shortens links, so a post made from the kit also verifies the X account, as does any other post from the tester's own profile that contains the code; the kit says so until X is verified. On Reddit the kit opens a text post with the link alone and no title, and tells the tester to post it once, on their own profile or in one community whose rules allow referral links: Reddit removes a text repeated across accounts and communities and filters links by domain, the link carries the referral by itself, and a Reddit account is verified from the account page, not by a post. On Telegram it sends the link and one line to a chat the tester chooses. With `EMISSIO_X_QUOTE_POST` set to the project's official post on X, the kit also offers to quote it, with the tester's link and code above it: the official post introduces Sequentia and Emissio, and a plain repost carries no link, so it brings the tester no referrals. Nothing is credited for quoting, reposting, liking or following: the referral bonus comes from a friend who joins through the link and completes tasks. The kit explains the quote step by step for someone who has never quoted a post: the button, and the same by hand from the official post (repost icon, Quote, paste the link and code it copies). A value that is not a link to a post hides the button. A visitor who arrives through a referral link sees, on the registration page and before the form asks for anything, who invited them and why the program is safe to try: nothing to buy or deposit, no keys asked, every task checked on the public chain, the code open source, and a referral bonus that pays only after real work. So a tester's post needs to say only what they think.

### Payout address

You register a Sequentia mainnet receiving address on your account page. Sequentia's transparent addresses use Bitcoin's own bech32/bech32m encoding, so a mainnet address starts with `bc1` (segwit v0 or v1), and any audited Bitcoin wallet can generate one; the in-app guide explains safe key generation. Validation does a full checksum check and gives specific errors for testnet (`tb1`), confidential (`sqb1`), Liquid, and legacy formats.

### The payout

Balances are paid in one payment from the paying wallet, straight to each account's wallet: a payout run. Each account's allocation is a share of its own that its wallet collects by itself the next time it opens; what is not collected by the run's deadline (a few weeks after the payment) goes back to the paying wallet, and the run records each account as collected or returned.

Each account is named by a public key its wallet holds:

- **The launch payout**, when it is paid this way, names the key of the registered mainnet address. A taproot address (`bc1p…`) is its key. An address starting `bc1q` names only a hash of its key, so with `EMISSIO_PAYOUT_LAUNCH_KEY=1` the account page asks for its public key once (66 characters, `02…` or `03…`) and checks it against the address; off (the default), the page asks for nothing, as an on-chain payment to the address needs no key. A script address (a `bc1q` of 62 characters) cannot be paid this way.
- **The testnet rehearsal** names the key of the first receive address of the account's Sequentia testnet wallet. The account page asks for it only while an admin has the rehearsal asking for wallets (`/admin/payouts`), and keeps showing it to an account that gave one or was paid; to anyone else the page shows nothing of the rehearsal. An account that linked its web wallet (the link task: a message signed in the wallet's Sign tab) is paid at the linked receive address's key, which the wallet looks for payments under. Otherwise the account pastes its wallet's account key (a `tpub`, with or without its `[fingerprint/84h/1h/0h]` origin, or the descriptor lines): Emissio derives the key of `m/84'/1'/0'/0/0` from it and keeps only that key. An address of the wallet that has already sent a transaction works too: its key is read from that spend through the explorer (`EMISSIO_ESPLORA`). A public key or a taproot address is taken as it is. A rehearsal pays tSEQ and moves nothing in any ledger.

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

The queue holds only what a check cannot decide. Task evidence that passes its check is credited at once, evidence that fails it is refused at once with the reason, and evidence that has not arrived yet waits and is credited by the check when it does; a pass, of task evidence or of a verification, waits for a reviewer only when its account is held or shares an uncleared signal with another. What reaches an admin: task evidence the check could not run on or cannot judge, passing evidence and verifications of a held account or one that shares an uncleared signal, verifications the check cannot judge, bug and security reports (test reports aside), and competition entries, which admins judge. Admin accounts are ordinary accounts with an admin flag, created from the server command line; there is no self-service admin signup.

Anti-farming, mechanically enforced in the database:

- Approval credits the ledger inside the same transaction as the status change, and partial unique indexes on the ledger make double credits impossible (one credit per submission, entry, report, verification, and referral).
- One transaction id can be evidence for one account only.
- One social account can verify one Emissio account, ever, and one Emissio account holds one live verification per platform: requests of one account and platform run one at a time, and a partial unique index refuses a second live row.
- One network address creates at most three accounts a day, and one browser (a long-lived device cookie) at most three accounts ever.
- The network addresses and browsers each account signs in and submits from, and the chain addresses its passing evidence was funded from (the faucet's and Emissio's own left out), are recorded; accounts sharing any of them, or a payout address, are linked on each other's admin page and counted beside them in the queue and the user list.
- Earning waits for a confirmed email address when the instance sends mail, and Emissio pays only accounts that completed the first-transaction task.
- An admin can hold an account: nothing it submits is approved automatically, it earns no referral reward, and payout runs leave it out with that reason.
- Manual admin ledger adjustments and revocations carry a reason the account sees in its ledger, and every admin action on an account is logged under the admin who took it.

### The admin desk

`/admin/queue` is one queue for everything waiting on a person, oldest first, filterable by kind: task evidence (approved, rejected or re-checked in place), bug reports, security reports, verifications, and the entries of competitions past their closing date. Each row names its account with its signals and whether it is held. Below the queue are the latest rewards the checks credited without a reviewer, each revocable with a reason.

Every issue the project already knows of is a GitHub issue labelled `known-issue` on the tracker of the ConcatenaLabs repository whose code has it, so one search lists them all: [`org:ConcatenaLabs label:known-issue`](https://github.com/search?q=org%3AConcatenaLabs+label%3Aknown-issue&type=issues). The queue links it; check a bug or security report against it before accepting one, because a report of a known issue is a duplicate.

`/admin/users` searches accounts by email, account code, network address, payout address, a chain address their evidence touched, a txid they submitted or the account id; each account's page shows its balance, signals and linked accounts (and what they share), where it was seen, its submissions (approved ones revocable), bug reports, ledger with an adjustment form, the hold, a password reset link, and the log of admin actions on it.

The task editor (`/admin/tasks`) changes a task's copy, stage (its category), reward, cap, surface, check, sort and active flag, or adds a task, with no deploy. Exports: `/admin/allocations.csv` (the launch allocation: `user_id, email, mainnet_address, balance_seq, verified_platforms`) and `/admin/export/users.csv`, `submissions.csv`, `ledger.csv`, `bugs.csv`; a cell a spreadsheet would run as a formula is written as text.

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

`wallet` is one of `web-wallet`, `ambra-browser`, `ambra-mobile`; `description` is required. `"test": true` (or a description starting with `EMISSIO-TEST`) marks a report filed by an automated check: it is stored as a test report, kept out of the review queue and out of rewards. `account_code` is optional and links the report to the account with that code; a code that matches no account is kept with the report, which stays unattributed. Longer fields are cut to their limits (description 4,000 characters, version 40, mode 20, 20 errors of 500 characters each, the newest kept, context values 300, 20 addresses of 120) and unknown fields are ignored. Keys and seeds are never part of a report.

| Answer | Body | When |
|---|---|---|
| `201` | `{"id": 123, "account": "linked"}` (or `"unknown"`, `"none"`; with `"test": true` for a test report) | stored |
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
| `GET /r/{code}` | Referral link (sets a 30-day cookie, redirects to registration, which then names the inviter) |
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

Running `createadmin` for an existing email resets that account's password and makes it an admin; a new admin's address counts as confirmed. The other subcommand, `./emissio reseed-tasks`, refreshes the title, category (its stage), body, surface, check and place on the path (`sort`) of the seeded tasks from the current `seed.go` in an existing database, inserts any seeded task the database does not have yet, and deactivates the tasks listed as retired in `seed.go`, leaving admin-tuned rewards, caps, and active flags of the other existing tasks alone. An inactive task, whether an admin switched it off or it was retired, is left out of the task page, and its own address shows only its title and that it is closed, with no instructions and no form; a submission to it is refused. The path's order is the seed's, so a sort an admin set on a seeded task is replaced; a task an admin added keeps its own.

Tests:

```
go test ./...
```

`app_test.go` drives the full HTTP surface against a temporary database: registration, submission and review lifecycle, prize and report awards, referral qualification, verifications (with stubbed Reddit/Telegram servers), duplicate-txid rejection, base-path handling, CSRF, and anonymous-access denial. `bugs_test.go` covers the bug page (refusals, the screenshot's privacy, acceptance by severity paying once, duplicate merging) and the wallet intake (every answer of its contract, the limits, attribution by account code). `checks_test.go` covers every check's verdicts against a stand-in explorer and stand-in services (the staking-script parsers against scripts the node itself built, in every form), the release check against a stand-in peer list, the submission form's three outcomes and the dashboard. `regtest_tasks_test.go` runs the counterparty tasks against a real node on a private chain: Emissio's wallet pays, a tester's wallet returns or unblinds, and the checks read the transactions back, the wrong-coin and wrong-fee-asset returns and a wrong amount refused; it is skipped unless `EMISSIO_REGTEST_TASKS_CLI` and `EMISSIO_REGTEST_TASKS_DATADIR` name the node (the test's header says what it must hold). `feetasks_test.go` covers the fee-list checks' account values, every verdict and the task pages' values. `home_test.go` covers the front page, the path's order (`TestSeededTasksFollowThePath`) and `reseed-tasks` putting an existing database on it. `regtest_feelist_test.go` runs the fee-list tasks on two private-chain nodes, a tester's and a producer's: a fresh node refusing an unlisted fee asset, USDX listed by hand and then by the real price server (`--once --dry-run`, then `--once`), each transaction relayed and mined by the producer, and the checks reading them back, a fee at the wrong fee rate, at the network's rate and at the other task's price refused; it is skipped unless the variables its header names are set. `ownbitcoin_test.go` covers every verdict of the own-Bitcoin-node check, the parsing of a pasted status, and that the task is seeded switched off. `regtest_ownbitcoin_test.go` runs it end to end on a private network: two regtest Bitcoin nodes, the node repository's shared-endpoint proxy, a public Sequentia node and the tester's, first on the shared endpoint (refused) and then on its own Bitcoin node (refused while the endpoint's record of it is fresh, then passed), with the peer list written by the ops repository's `bin/btc-peers.py`; a program that only completes the handshake refused, a second account from the same address sent to review, a record younger than the quiet interval answered with when to try again, and a full record sent to review; it is skipped unless the variables its header names are set. `closedtask_test.go` covers an inactive and a retired task on their own addresses: closed, with no instructions and no form, signed in or not, and a submission refused and not stored. `walletlink_test.go` recovers the key from signatures a Sequentia node made, and covers the link task's refusals and the one-account rule. `arcawallet_test.go` drives every web wallet task against a stand-in `arca` (`testdata/fake-arca.py`, answering in the command line's shapes): attribution by a coin's record, a stray coin, the round trip's wrong amounts, a return short by a transfer's margins, the exact one, the submission form staying closed until a check passes, the Bitcoin ark address, invoice limits, the swap's proof decimals. `arcarecord_test.go` reads coin records copied from real arca wallets of a private-chain run (`testdata/arca-records.json`: both formats, batch leaves, a board, fills, and transfers whose inputs are a board, a fill or another transfer) and checks that the nonce read for each coin's own leaf is the one its wallet keeps, that a transfer names its own leaf and not its input's, that a damaged or unknown record is not read, and that a code's key and nonce in an output's script are never taken for the owner. `review_round5_test.go` covers a coin whose history paid another code paying the code its own leaf names, bytes in a script paying nothing, Emissio refusing its own codes in the round trip and by a payment name, the bot passing the email gate, a record failing more than thirty times and then read, with the desk naming it meanwhile, a record in an unknown format kept unread, parallel requests making one code and at most three payments, a stranger's message filing nothing on a held account while the owner's form does, a failed write leaving the coin unread, an older payment read while new coins arrive faster than a follow reads, the sync's own coins read first, quiet follows not listing the coins, the form and the bot racing for one Telegram ID, the bot telling a stranger nothing of an account, a dead poller on the desk, and codes corrected by hand keeping their payment with the tester's real coin recorded. `reconcile_test.go` covers the reconciliation against the same stand-in: a coin accepted outside the follower's own sync matched on the next follow across a restart and its waiting submission credited once, its record read once; a new code refused for ten minutes and then given, at most five a day; a payment to the older of two codes counted, one coin paying one code, a second coin to a paid code kept as not counted and shown on the desk; the desk's note before the coins are read, with records still to read, and on a rejected submission; the cost of a follow over a thousand coins (one list, at most 50 records, the newest first, a coin of no code logged once); a record that fails read again later, and a failed sync reconciled all the same. `tgbot_test.go` drives the bot against a stand-in Bot API: a message verifying an account with no form and the bot's answer, no code and an unknown code answered and nothing kept, one Telegram account for one Emissio account (a second account's code, a new username, the schema's guard, a pending request), more than a hundred messages, the Bot API down for an hour, unreachable or refusing the token with no error carrying the token, a message asked for again not handled twice, an ID with no age to vouch for answered with the form's way in and filed by the owner, a young account and a stale message, and the ID taken from the notes of verifications made before it was recorded; every test fails if the token reaches the log. `queue_test.go` covers what keeps the review queue to what needs a person: a submission whose evidence has not arrived waiting and credited by the poller (once), refused when its check later fails, lapsing after its time, and sent to a reviewer only when its check cannot run at the end; a new submission replacing a waiting one; a held account's evidence going to a reviewer; a clean verification approved at once, one whose account shares a network address queued with that note, young accounts, missing codes and missing posts refused and not stored; the account page naming the bot; and test bug reports kept out of the queue and of rewards. `review_round_test.go` covers the races and timings around an automatic credit: two verification requests of one account and platform at once (one paid, the other refused, a second live row refused by the database), a hold placed while a check runs on the waiting poller, the submit path and a verification, a network address, browser or chain address shared after a submission began to wait (sent to a reviewer with the signal, then credited once a reviewer clears the link, and a new link weighed again), a judgment case met by the poller queued at once and never lapsed, an unconfirmed transaction without the account's proof decimals refused and a waiting row never keeping the transaction's owner out, and the test marker matched as a whole word. `review_round3_test.go` covers what a clearance covers (the cleared network address alone credits; a new network address, browser or chain address between the same pair queues again; the rows and the log name the admin), the funding taken from the checked transaction on the submit and poller paths while the explorer fails after the check's read, a transaction check with no transaction at hand failing closed, `readsTx` agreeing with which checks read a transaction, and a referral between linked accounts held for a reviewer, then paid once on approval or never on rejection. `review_round4_test.go` runs many qualification checks of one referral at once and rejects it, or pays it and links the pair, while they are in flight (a rejected referral stays unpaid and a paid one gains no review row), closes a review row of a referral already paid as paid, and covers a hold on either side of a referral item: shown, and never paid. `desk_test.go` covers the registration limits by network and by browser and the signals they leave, the queue across every kind, approving and revoking (once, with a reason the tester sees), reasoned adjustments, holds, account search, and the exports. `mail_test.go` covers confirming the address (the gate, resend limits, only the newest link working), password reset (the same answer for unknown addresses, a link that works once, sessions ended), admin reset links, and the SMTP exchange itself against a stand-in server, credentials only after STARTTLS. `bech32_test.go` covers address validation and formatting helpers. `walletkey_test.go` covers reading a wallet's key from its account key (BIP84's own vector) and from an address that has spent, and that the rehearsal shows only when asked. `payout_test.go` covers payout keys, a rehearsal run against a stand-in payment service (left-out reasons, every refusal of a reference, collected and returned allocations, no balance moved) and the launch's single debit. `regtest_payout_test.go` runs the rehearsal end to end against a real payment service: an Arca operator from arca's `arca_operator_for_browsers` harness, named by `EMISSIO_REGTEST_CONTROL`, with `EMISSIO_REGTEST_ARCA` the `arca` binary; it is skipped without them.

### Configuration

All configuration is environment variables (`loadConfig` in `main.go`):

| Variable | Default | Meaning |
| --- | --- | --- |
| `EMISSIO_LISTEN` | `127.0.0.1:8095` | Listen address |
| `EMISSIO_DB` | `emissio.db` | SQLite database path |
| `EMISSIO_BASEPATH` | empty | Path prefix when served under one, e.g. `/emissio` |
| `EMISSIO_ESPLORA` | empty | Esplora API base URL for the transaction checks, e.g. `https://sequentiatestnet.com/api` (empty disables them; submissions then go to manual review) |
| `EMISSIO_POLICY_ASSET` | the testnet's tSEQ id | Asset id of tSEQ, for the fee and stake checks |
| `EMISSIO_USDX_ASSET` | the testnet's USDX id | Asset id of USDX, the fee asset of the fee-list tasks |
| `EMISSIO_BTC_PEERS` | empty | The box Bitcoin node's peer list, written by a timer on the box; with `EMISSIO_ANCHOR_SEEN`, enables the own-Bitcoin-node check |
| `EMISSIO_ANCHOR_SEEN` | empty | The shared Bitcoin endpoint's record of each address's last call, e.g. `http://127.0.0.1:48400/last-seen` |
| `EMISSIO_CLI`, `EMISSIO_DATADIR` | empty | `sequentia-cli` path and the public node's data directory; enables the peer-based checks, the ten-minute sighting poller and Emissio's wallet |
| `EMISSIO_COMPAGES` | empty | Compages' API base, for the bridge checks (empty sends bridge evidence to review) |
| `EMISSIO_POOLS` | empty | The pool board's feed, for the delegation checks (empty sends them to review) |
| `EMISSIO_SUMS` | empty | The download page's checksum list, for the release check |
| `EMISSIO_FAUCET_ADDRESSES` | the testnet faucet's covenant address | Comma-separated addresses the faucet pays from, for the faucet check |
| `EMISSIO_WALLET_URL` | `https://sequentiatestnet.com/wallet/` | The web wallet the front page sends testers to |
| `EMISSIO_X_QUOTE_POST` | (none) | the official X post the invite kit offers to quote; unset, no quote button |
| `EMISSIO_CONNECTORS` | `webwallet` | The wallets the link task offers to connect, in order, comma-separated: `webwallet` (the web wallet in another tab of this origin), `extension` (the browser extension), `phone` (a wallet on a phone, paired by a code), `local` (Sequentia Core through the `sequentia-connect` daemon). The page's content security policy lets it reach the pairing relays and the daemon's loopback port only while `phone` and `local` are offered |
| `EMISSIO_LNADDR` | empty | The payment-name service the payment-name task resolves names at, the LSP's base URL (for example `https://sequentiatestnet.com/lsp`); empty turns that task off |
| `EMISSIO_LNADDR_DOMAIN` | `sequentiatestnet.com` | The domain of the payment names Emissio pays |
| `EMISSIO_PRICES` | `https://sequentiatestnet.com/prices` | The price feed (USD per unit, by ticker) that sets the invoice limit in a Sequentia asset; empty: BTC invoices only |
| `EMISSIO_REGISTRY` | `https://sequentiatestnet.com/registry` | The asset registry that names an asset's ticker and precision, read as `<url>/<asset id>`; empty: BTC invoices only |
| `EMISSIO_ARCA`, `EMISSIO_ARCA_DATADIR` | empty | The `arca` command line and Emissio's wallet directory for the web wallet tasks (empty turns the counterparty ones off). The wallet is made with `arca create` against the testnet's operator and funded by boarding, as the faucet's leaf wallet is; keep it free of channels, so payments to its receive requests arrive as coins it can attribute |
| `EMISSIO_WALLET` | empty | Name of Emissio's own wallet on that node, which pays the counterparty tasks (empty turns them off) |
| `EMISSIO_WALLET_ASSETS` | empty | Comma-separated asset ids the wallet may pay in; empty: any it holds |
| `EMISSIO_WALLET_DAILY_CAP` | `200` | Payments the wallet makes in a day, in all |
| `EMISSIO_WALLET_FEE_RATE` | `2` | Fee rate of its payments, in reference units per vbyte; the node converts it into the paid asset |
| `EMISSIO_SECURE` | `0` | Set `1` behind HTTPS so cookies are marked Secure |
| `EMISSIO_SMTP_HOST`, `EMISSIO_SMTP_PORT` | empty, `587` | SMTP submission service for verification and reset mail; with a sender set, mail is on. Port 587 uses STARTTLS (required before credentials are sent), 465 implicit TLS |
| `EMISSIO_SMTP_USER`, `EMISSIO_SMTP_PASS` | empty | Its credentials; keep the password in `/etc/emissio/emissio.env` (mode 600), never in the repo or a unit file |
| `EMISSIO_MAIL_FROM` | empty | Sender, e.g. `Sequentia Emissio <emissio@sequentiatestnet.com>` |
| `EMISSIO_PUBLIC_URL` | derived from the request | Public base address used in mailed links, e.g. `https://sequentiatestnet.com/emissio` |
| `EMISSIO_TG_BOT_TOKEN` | empty | Telegram bot token (placeholder: `123456:ABC-...`); runs the verification bot's poller: automatic Telegram ownership and ID-based age checks |
| `EMISSIO_TG_BOT_NAME` | empty | The bot's username (without `@`), shown in user instructions; empty: read from the Bot API |
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

Mail goes out through an SMTP submission service: any transactional provider (Amazon SES, Postmark, Brevo, Mailgun and the like) works. For mail from `sequentiatestnet.com` to be delivered rather than filed as spam, the domain's DNS needs the provider's records: its SPF include merged into the existing SPF record (one `v=spf1` record per domain, so `v=spf1 include:spf.efwd.registrar-servers.com include:<provider> ~all`), the DKIM records the provider issues for the domain, and a DMARC record (`_dmarc` TXT, `v=DMARC1; p=none` to start). On the host, the SMTP password lives in `/etc/emissio/emissio.env` (mode 600), which the unit reads with `EnvironmentFile=`; `EMISSIO_SMTP_HOST`, `EMISSIO_SMTP_USER`, `EMISSIO_MAIL_FROM` and `EMISSIO_PUBLIC_URL` can sit beside it, and the host must reach the provider on port 587. One account gets at most three mails an hour, one network address causes at most ten, and a link works once: a day for confirming the address, an hour for a reset (a day for a link an admin makes).

Keep real secrets (such as the bot token) out of the repo and out of every unit file and drop-in: systemd shows an `Environment=` value to every user on the machine (`systemctl show`), whatever the file's mode. On a systemd host they go in `/etc/emissio/emissio.env`, mode 600, which the unit reads.

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
| `telegram.go` | Telegram checks (by the bot's kept messages, or the t.me bio) and ID-based account-age estimation |
| `tgbot.go` | The verification bot: the poller, its answers, verification on the message alone, its health on the desk |
| `arcarecord.go` | Reads an Arca coin record's own leaf, field by field |
| `esplora.go` | Explorer txid check |
| `checks.go` | The task checks and their verdicts: staking scripts read from their bytes, the faucet, Compages, the release |
| `waiting.go` | Waiting submissions: the poller that runs their check again, credits, refuses, queues or lapses them |
| `autocredit.go` | The automatic credit boundary: the hold and the uncleared shared signals read inside the approval, and a reviewer's clearance of links |
| `counterparty.go` | Emissio's own wallet: paying a tester, the return and unblind checks |
| `dashboard.go` | The tester's dashboard on the account page |
| `signals.go` | Addresses each account's evidence touched, for the admin's clustering signals |
| `walletlink.go` | Linking a wallet by a signed message: key recovery, its addresses |
| `arcawallet.go` | Emissio's arca wallet: receive codes, paying codes and invoices, the follower and its reconciliation, the web wallet checks, the desk's note |
| `invoicelimit.go` | The most Emissio pays one invoice in each asset, and the asset an invoice is paid in |
| `receivecode.go` | Reads a wallet's receive code: its parts, its address, its short Sequentia request expanded |
| `desk.go` | The admin desk: the queue, account search and pages, signals, holds, revocations, exports, registration limits |
| `bugs.go` | Bug reports: the form, screenshots, the wallet intake, the admin review |
| `mail.go` | Mail: the SMTP sender, confirming the address, password reset, admin reset links |
| `payout.go` | Payout runs: keys, the run's list, the reference check, the follower, the admin's and the account's pages |
| `walletkey.go` | Reading what an account pastes (an account key, an address that has spent, a key) into the key the rehearsal pays |
| `templates/`, `static/` | HTML templates and assets, embedded into the binary |
| `deploy/` | systemd unit and box install script |

### Data storage

One SQLite file (WAL mode, foreign keys on, a single connection to serialize writers). Tables: `users`, `sessions`, `tasks`, `submissions`, `competitions`, `entries`, `reports`, `ledger`, `verifications`, and for the payout `payout_keys`, `payout_runs`, `payout_rows`, `payout_settings`, for bug reports `bugs` and `bug_files` (screenshots), `email_tokens` for the hashes of mailed links, `emissio_payments` and `kv` for Emissio's wallet (its payments, its return address), `chain_links` for the addresses each account's evidence touched, `user_ips` and `user_devices` for where each account was seen, `cleared_signals` for the shared signals a reviewer cleared, `referral_reviews` for referrals held for a reviewer, and `admin_log` for admin actions, `wallet_links` for linked wallets, `arca_codes` and `arca_sends` for the receive codes Emissio's arca wallet made and the payments it sent, `arca_coins` for each coin of that wallet whose record was read and the code it paid, and `tg_messages` for a day of the verification bot's messages that carry an account code. Every reward is a ledger row; a balance is `SUM(amount)` over the user's rows. Ledger kinds: `submission`, `revocation` (a revoked submission's reward, negative), `entry`, `report`, `bug`, `verification`, `referral`, `referral-welcome`, `adjustment`, `payout` (a collected launch allocation, negative), and the reserved `presale`. Partial unique indexes on `(kind, ref_id)` are the double-credit guard.

### Deployment

The live instance runs as a systemd service behind Caddy on the sequentiatestnet.com host. `deploy/emissio.service` is the unit: it runs `/opt/emissio/emissio` as the `emissio` system user, which owns `/var/lib/emissio` (the database, mode 600) and can write nothing else, with `EMISSIO_BASEPATH=/emissio`, local electrs as the esplora endpoint and the secrets from `/etc/emissio/emissio.env`. `deploy/install-on-box.sh` builds, makes the user, installs the binary and the unit, restarts the service and adds the Caddy route; run it again to deploy a new revision. Anything the service reads on the host (a `sequentia-cli` for `EMISSIO_CLI`, a node configuration for `EMISSIO_DATADIR`, an arca data directory) must be readable by that user, so none of it can sit under `/root`. It assumes Go at `/root/toolchains/go/bin` and the box's canonical Caddy site block; on any other host, follow the steps in the header of `deploy/emissio.service` by hand. The route it adds:

```
redir /emissio /emissio/ permanent
handle_path /emissio/* {
    reverse_proxy 127.0.0.1:8095
}
```

Caddy's `handle_path` strips the `/emissio` prefix before proxying, but the app still needs `EMISSIO_BASEPATH=/emissio` to generate correct links; it strips the prefix itself if present, so both stripped and unstripped proxy setups work.

Before every deploy, check that no account holds two live verifications of one platform. The service creates the unique index `verifications_one_live` when it starts, and a database holding such a pair cannot take it: the service then runs without the index (only its in-process lock keeps one request per account and platform) and logs a warning naming the index. The box has no `sqlite3` binary, so read the database with Python, read-only, as the service's user:

```sh
runuser -u emissio -- python3 -c "import sqlite3; c=sqlite3.connect('file:/var/lib/emissio/emissio.db?mode=ro', uri=True); print(c.execute(\"SELECT user_id, platform, COUNT(*) FROM verifications WHERE status != 'rejected' GROUP BY 1,2 HAVING COUNT(*) > 1\").fetchall())"
```

It prints `[]` when the database is clean. Otherwise each `(account, platform, count)` it lists needs all but one of those verifications rejected from the admin desk before the deploy. After the restart, `journalctl -u emissio | grep verifications_one_live` shows the warning if the index was still skipped.

Contributions: PRs against `main`.

## License

MIT, see `LICENSE`.
