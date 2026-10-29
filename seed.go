package main

import (
	"database/sql"
	"log"
	"time"
)

// Program constants. Amounts are whole SEQ.
const (
	programPool    int64   = 32_285_714
	launchPriceUSD float64 = 0.175

	// Security awards. securityReserve is the share of the pool set aside
	// for security awards; securityMax is the ceiling for one exceptional
	// finding, and the upper bound the admin review form accepts.
	securityReserve int64 = 16_000_000
	securityMax     int64 = 1_500_000

	// Fingerprint of the team key that encrypted reports are addressed to.
	// The public key is static/security-pgp.asc; the private key never
	// touches the server.
	securityPGPFingerprint = "175E 5689 7EFA 18DF 9D6A  E5BD 8A92 51CD 2A22 27AF"
)

// Security reward tiers (whole SEQ), shown on the security page and used as
// defaults in the admin review form.
var securityTiers = []struct {
	Name   string
	Reward int64
	Desc   string
}{
	{"Low", 1_000, "A reproducible defect in shipped code with a demonstrated security consequence, however small: a crash any peer can trigger on one node, an RPC or wallet path that leaks something it should not, a fee-asset or blinding edge case that loses testnet funds under a specific sequence. Documentation errors, UI problems, missing headers, version disclosure and theoretical concerns are not security findings; report those as a bug instead."},
	{"Medium", 6_000, "A contained vulnerability with a working reproduction: remote denial of service against a single node, an authentication or authorization weakness in an RPC or web surface, a wallet bug that loses funds under conditions a normal user can hit."},
	{"High", 30_000, "Serious vulnerabilities: remote crash of many nodes, theft of funds requiring user interaction, breaking the opt-in confidentiality of a blinded transaction."},
	{"Critical", 150_000, "Network-level vulnerabilities: consensus splits, silent inflation of any asset, theft of funds without user interaction. A working proof of concept is expected at this tier."},
}

type seedTask struct {
	slug, title, category, body string
	surface, check              string
	reward, cap                 int64
	needsTxid                   bool
	sort                        int64
}

// retiredTasks are seeded tasks that reseed-tasks deactivates: the products
// they need are not part of the public testnet, or the bug page took the
// task's place.
var retiredTasks = []string{"seqdex-swap", "cross-chain-swap", "lightning-swap", "report-bug"}

// surfaces are the public testnet's surfaces, in the order the dashboard
// shows a tester's progress through them.
var surfaces = []struct{ Key, Name string }{
	{"core", "Sequentia Core"},
	{"explorer", "Block explorer"},
	{"faucet", "Faucet"},
	{"compages", "Compages bridge"},
	{"pools", "Staking pools"},
	{"emissio", "Emissio"},
}

func surfaceTitle(key string) string {
	for _, s := range surfaces {
		if s.Key == key {
			return s.Name
		}
	}
	return key
}

var seedTasks = []seedTask{
	{
		slug: "first-transaction", title: "Make your first testnet transaction", category: "Getting started", surface: "core", check: "first-transaction",
		reward: 10, cap: 5000, needsTxid: true, sort: 10,
		body: `Install Sequentia Core, claim tSEQ from the faucet, and send an amount that proves the transaction is yours.

- Download Sequentia Core from sequentiatestnet.com/download/core/ (Linux or Windows) and start it. It connects to the testnet on its own; let it sync.
- In the wallet, choose Receive and create an address (it starts with tb1). From a node, sequentia-cli getnewaddress does the same.
- Claim tSEQ at sequentiatestnet.com/faucet/ to that address.
- Send exactly 1.{PROOF} tSEQ to another address. A second address of your own is fine. The eight decimals are your proof decimals: they are how we know the transaction is yours, and every transaction you submit for any task must carry them in one of its outputs.
- Once it has confirmed, submit the txid. You find it in the wallet's transaction list, and at sequentiatestnet.com/explorer/.

The check is automatic: a confirmed transaction with your proof decimals is credited on the spot.`,
	},
	{
		slug: "anchor-lookup", title: "Find the Bitcoin anchor of your transaction", category: "Getting started", surface: "explorer", check: "anchor-lookup",
		reward: 10, cap: 3000, needsTxid: true, sort: 15,
		body: `Every Sequentia block commits to a Bitcoin testnet4 block header, and Sequentia reorganises whenever Bitcoin does. Trace that link for one of your own transactions.

- Take a confirmed transaction with an output of 1.{PROOF} tSEQ (your first-transaction send qualifies).
- Open it in the explorer at sequentiatestnet.com/explorer/ and go to the block that contains it. The block page shows the Bitcoin testnet4 block it anchors to.
- Submit the txid, and in the notes paste the anchored Bitcoin block hash.

The hash in your notes is compared with the anchor recorded for that block. If the explorer makes this hard to find, file a bug report: that is exactly the feedback the testnet is for.`,
	},
	{
		slug: "faucet-consolidate", title: "Combine two faucet payments in one transaction", category: "Getting started", surface: "faucet", check: "faucet-consolidate",
		reward: 15, cap: 2000, needsTxid: true, sort: 18,
		body: `Claim tSEQ from the faucet twice, then spend both coins together.

The faucet pays tSEQ from a covenant that allows one payment at a time and a fresh claim only after a wait, so the second claim takes patience. If the faucet refuses a claim, it says why and when to try again: note anything about that message that is unclear.

- Claim tSEQ at sequentiatestnet.com/faucet/ to an address of yours, and again later to another address of yours.
- Send both coins in one transaction: in Sequentia Core, use coin control to select both, or send an amount larger than either coin alone. Make one output exactly 1.{PROOF} tSEQ, and the rest can return to you as change.
- Once it has confirmed, submit the txid.

The check is automatic: the transaction must carry your proof decimals and spend coins from two faucet payments.`,
	},
	{
		slug: "issue-asset", title: "Issue your own asset", category: "Assets", surface: "core", check: "issue-asset",
		reward: 20, cap: 2500, needsTxid: true, sort: 20,
		body: `Issue a new asset on the Sequentia testnet.

On Sequentia, anyone can issue an asset, and every issued asset has equal standing on the chain. Issue one of your own: a point system, a voucher, a test stablecoin, anything.

- Issue the asset from the desktop wallet, or with the issueasset RPC on your node, with an amount that ends in your proof decimals: for example 1000.{PROOF}. Ask for a reissuance token as well: the next task uses it.
- Once it has confirmed, submit the issuance txid. In the notes, tell us what the asset is meant to be, if you like.

The check is automatic: a new issuance with your proof decimals on the issued amount.`,
	},
	{
		slug: "reissue-asset", title: "Reissue an asset you created", category: "Assets", surface: "core", check: "reissue-asset",
		reward: 20, cap: 1000, needsTxid: true, sort: 25,
		body: `Increase the supply of an asset you issued, using its reissuance token.

- Take the asset you issued in the previous task; the reissuance is matched against that issuance.
- Reissue an amount ending in your proof decimals, for example 500.{PROOF}, from the desktop wallet or with the reissueasset RPC.
- Once it has confirmed, submit the reissuance txid.

The check is automatic: a reissuance, with your proof decimals, of an asset whose issuance you submitted.`,
	},
	{
		slug: "burn-asset", title: "Burn part of an asset you issued", category: "Assets", surface: "core", check: "burn-asset",
		reward: 15, cap: 1000, needsTxid: true, sort: 27,
		body: `Destroy part of the supply of your own asset, provably and for good.

A burn sends an amount to an output nobody can ever spend, and the explorer counts it out of the asset's supply.

- Burn an amount of the asset you issued that ends in your proof decimals, for example 1.{PROOF}, with the destroyamount RPC on your node.
- Look the asset up in the explorer and check that its supply went down by that amount. If it did not, file a bug report.
- Once it has confirmed, submit the txid of the burn.

The check is automatic: a burn of an asset your account issued, of an amount ending in your proof decimals.`,
	},
	{
		slug: "any-asset-fee", title: "Pay a fee in an asset other than tSEQ", category: "Assets", surface: "core", check: "any-asset-fee",
		reward: 15, cap: 2500, needsTxid: true, sort: 30,
		body: `Send a transaction whose fee is paid in an issued asset instead of tSEQ.

Sequentia has an open fee market: fees can be paid in any accepted asset, and tSEQ has no special status beyond staking. Claim USDX, GOLD or another asset from the faucet, then send a transaction and choose that asset as the fee asset.

- In the desktop wallet the fee asset is a choice on the send screen; from a node, pass fee_asset_label to sendtoaddress.
- Make one output an amount ending in your proof decimals, for example 1.{PROOF} of whatever you send.
- Once it has confirmed, submit the txid.

The check is automatic: the proof decimals, and a fee output in an asset other than tSEQ.`,
	},
	{
		slug: "multi-asset-send", title: "Send two assets in one transaction", category: "Assets", surface: "core", check: "multi-asset-send",
		reward: 15, cap: 2000, needsTxid: true, sort: 35,
		body: `Send two different assets to another address in a single transaction.

- Hold at least two assets, for example tSEQ and USDX from the faucet, or an asset you issued.
- In the desktop wallet, add a second recipient and choose a different asset for it; from a node, sendmany with an assets argument does the same. Make one of the amounts end in your proof decimals, for example 1.{PROOF}.
- Once it has confirmed, submit the txid.

The check is automatic: outputs in two assets, and your proof decimals.`,
	},
	{
		slug: "confidential-tx", title: "Send an opt-in confidential transaction", category: "Assets", surface: "core", check: "confidential-tx",
		reward: 15, cap: 2000, needsTxid: true, sort: 40,
		body: `Sequentia is transparent by default, and confidentiality is opt-in. Generate a confidential address (it starts with tsqb) and send funds to it, so the amount is blinded on-chain.

- Generate a confidential address, for example with getnewaddress "" blech32 on your node.
- In one transaction, send any amount to the confidential address and 1.{PROOF} tSEQ to a transparent address of yours. The blinded output hides its amount, so the proof decimals ride on a transparent one.
- Once it has confirmed, submit the txid.

The check is automatic: a blinded output, and your proof decimals.`,
	},
	{
		slug: "emissio-return", title: "Return a payment to Emissio", category: "With Emissio", surface: "emissio", check: "emissio-return",
		reward: 25, cap: 1000, needsTxid: true, sort: 45,
		body: `Emissio pays you, and you pay it back exactly, with the fee in the asset you return.

Emissio holds a testnet wallet of its own. Ask it for a payment below: it sends an amount nobody can predict, of whichever asset it picks, to a transparent address of yours (tb1). Then return it.

- Give Emissio a tb1 address of your wallet and wait for the payment to arrive.
- Send exactly the amount you received, of the same asset, back to Emissio's address shown below, in a transaction that spends the coin Emissio sent you. In Sequentia Core, coin control selects it.
- Pay the fee in that same asset. You will need a little more of it than Emissio sent: the faucet hands out every asset Emissio pays in.
- Once it has confirmed, submit the txid below.

The check is automatic: the transaction spends Emissio's payment, returns the exact amount of that asset, and pays its fee in it.`,
	},
	{
		slug: "emissio-unblind", title: "Receive a confidential payment and unblind it", category: "With Emissio", surface: "explorer", check: "emissio-unblind",
		reward: 25, cap: 1000, needsTxid: false, sort: 47,
		body: `Emissio pays you an amount that only you can see, and you prove you can see it.

A payment to a confidential address hides its amount from everyone except the holder of the address's blinding key. Your wallet unblinds it for you, and so can the explorer, when you give it the blinding data.

- Give Emissio a confidential address of yours (tsqb, from getnewaddress "" blech32) below.
- Once the payment arrives, read its amount in your wallet's transaction list.
- Open the payment in the explorer at sequentiatestnet.com/explorer/ with the blinding data your node gives (sequentia-cli gettransaction shows the amount blinders; the explorer unblinds a transaction from a link ending #blinded=...). Check that it shows the same amount.
- Submit the amount, or the explorer link that unblinds it, in the notes below.

The check is automatic. A wrong amount counts as a try; after five, ask for a new payment.`,
	},
	{
		slug: "bridge-in", title: "Bridge an asset into Sequentia with Compages", category: "Bridge", surface: "compages", check: "bridge-in",
		reward: 30, cap: 1000, needsTxid: true, sort: 50,
		body: `Lock an asset on Ethereum Sepolia or Solana devnet and receive the bridged asset on Sequentia.

Compages, at sequentiatestnet.com/bridge/, locks the original in a vault and mints a Sequentia asset for it; later deposits of the same token reissue the same asset.

- Get some Sepolia ETH, or devnet SOL, from any public faucet for that network.
- On the bridge page, choose the asset and amount, and give a Sequentia address of yours as the destination. Wait for the bridged asset to be delivered.
- The delivery is made by the bridge, so it cannot carry your proof. Prove control instead: send some of the delivered coin to another address of yours, in a transaction that spends it, with one output ending in your proof decimals (for example 0.{PROOF}).
- Submit the txid of that send. In the notes, give the deposit: the Ethereum transaction hash (0x...), or the Solana deposit address the bridge gave you.

The check is automatic: the bridge's own records name the delivery for that deposit, and your transaction spends it with your proof decimals.`,
	},
	{
		slug: "bridge-both", title: "Bridge from the other source chain", category: "Bridge", surface: "compages", check: "bridge-both",
		reward: 30, cap: 500, needsTxid: true, sort: 55,
		body: `Bridge in from the chain your first bridge did not use: Solana devnet if you started on Sepolia, Sepolia if you started on Solana.

The two legs work differently: the Ethereum leg is a deposit into a vault contract from your browser wallet, and the Solana leg a transfer to a deposit address the bridge gives you. Each has its own ways to go wrong; tell us about any you meet.

- Follow the steps of the first bridge task on the other chain.
- Spend the delivered coin with an output ending in your proof decimals, and submit that txid with the deposit (hash or deposit address) in the notes.

The check is automatic, as for the first bridge, and also checks that this deposit came from the other chain.`,
	},
	{
		slug: "bridge-out", title: "Bridge an asset back out with Compages", category: "Bridge", surface: "compages", check: "bridge-out",
		reward: 40, cap: 500, needsTxid: true, sort: 60,
		body: `Return a bridged asset to its original chain.

The vault releases the original only after the return transaction's Bitcoin anchor is buried deep enough that a Bitcoin reorganisation cannot undo it. That wait is deliberate, and this task exercises it end to end.

- On sequentiatestnet.com/bridge/, create a redemption address bound to your Ethereum or Solana address.
- Send the bridged asset to that redemption address from your Sequentia wallet, with an amount ending in your proof decimals, for example 0.{PROOF}.
- Wait for the release on the source chain; the bridge page tracks it.
- Once it is released, submit the Sequentia txid of your return transaction. In the notes, say roughly how long the release took.

The check is automatic: the bridge's own records show your transaction at that redemption address, released.`,
	},
	{
		slug: "install-release", title: "Install the release and check its checksum", category: "Infrastructure", surface: "core", check: "install-release",
		reward: 15, cap: 2000, needsTxid: false, sort: 65,
		body: `Download Sequentia Core, verify the file before you run it, and connect it to the network.

- Download the release for your system from sequentiatestnet.com/download/core/, and the SHA256SUMS file next to it.
- Compute the file's SHA-256 (sha256sum on Linux, Get-FileHash in PowerShell on Windows) and compare it with the line in SHA256SUMS.
- Start the node with -uacomment=emissio-{CODE} (your account code) and -addnode=159.195.15.140:18444, and keep it running while you submit.
- In the notes, paste the checksum you computed.

The check is automatic: the checksum must match a file on the download page, and the public node must see your node, announcing your code, running that release.`,
	},
	{
		slug: "build-from-source", title: "Build Sequentia Core from source", category: "Infrastructure", surface: "core", check: "build-from-source",
		reward: 30, cap: 500, needsTxid: false, sort: 70,
		body: `Build the node from the source repository on your own machine and prove it by what the build announces to the network.

- Before building github.com/ConcatenaLabs/Sequentia, open src/clientversion.cpp and change the client name from "Sequentia Core" to "Emissio-{CODE}" (your account code). A release binary can add a comment to its user agent, but only a rebuild can change the name itself, so the name is the proof.
- Build sequentiad and sequentia-cli (the desktop wallet is optional) following the repository's build documentation.
- Run your build on the testnet with -addnode=159.195.15.140:18444 so it connects to the public node, and submit this task while it is running.
- In the notes, give your operating system and version, and anything in the build instructions that was wrong or missing.

The check is automatic: the public node's peer list must show a peer announcing /Emissio-{CODE}:...`,
	},
	{
		slug: "run-node", title: "Run a full node for a week", category: "Infrastructure", surface: "core", check: "run-node",
		reward: 50, cap: 1000, needsTxid: false, sort: 80,
		body: `Run a Sequentia testnet full node continuously for seven days, and let the network see it.

Full-node sovereignty is a core Sequentia principle: block producers cannot force rule changes on nodes that validate everything themselves.

- Run a full node with -uacomment=emissio-{CODE} (your account code, which the node announces to its peers) and -addnode=159.195.15.140:18444 so it stays connected to the public node.
- Keep it running for a week. The public node records every ten minutes which nodes announcing an Emissio code are connected.
- After seven days, submit this task.

The check is automatic: it counts the ten-minute slots of the last week in which your node was seen; a week's span with at least 80% of slots present passes.`,
	},
	{
		slug: "stake", title: "Stake tSEQ", category: "Staking", surface: "core", check: "stake",
		reward: 40, cap: 500, needsTxid: true, sort: 85,
		body: `Lock a stake and register as a potential block producer.

Only the Sequence token can stake, and the minimum is 40,000 tSEQ, which one faucet claim covers. A staking output keeps its weight for as long as it is unspent; the lock only gates withdrawal.

- In Sequentia Core, open the Staking tab and create a staking output of 40000.{PROOF} tSEQ (at least 40,000, ending in your proof decimals) with the shortest lock the chain allows. From a node, registerstake does the same: pass it your staker public key (getnewaddress, then getaddressinfo) and the amount.
- Keep the node running; the Staking tab shows your weight.
- Once it has confirmed, submit the txid of the staking output.

The check is automatic: a staking output of at least 40,000 tSEQ carrying your proof decimals. The next tasks build on this stake.`,
	},
	{
		slug: "delegate-stake", title: "Delegate your stake to a staking pool", category: "Staking", surface: "pools", check: "delegate-stake",
		reward: 30, cap: 500, needsTxid: true, sort: 87,
		body: `Lend your stake's block-signing rights to a pool without moving your coins.

A delegation is a separate record on the chain that points your staking output's key at a pool's signer. The coins never leave your wallet, the pool can never spend them, and you can re-point or withdraw the delegation at any time.

- Pick a pool on the board at sequentiatestnet.com/pools/. Read what it has committed to paying out.
- In Sequentia Core's Staking tab, delegate the stake from the previous task to that pool; from a node, delegatestake does the same. If you delegate a new amount instead, make it end in your proof decimals, for example 100.{PROOF}.
- Once it has confirmed, submit the txid of the delegation record. Check the board afterwards: your weight should appear under the pool.

The check is automatic: the record must name your stake's key (or the key of a new stake carrying your proof decimals in the same transaction) as its controller, and a pool on the board as its signer.`,
	},
	{
		slug: "redelegate", title: "Move your delegation to another pool", category: "Staking", surface: "pools", check: "redelegate",
		reward: 20, cap: 500, needsTxid: true, sort: 88,
		body: `Change your mind about a pool, the way a delegator should be able to: unilaterally, at once, without touching the stake.

- Pick a different pool on sequentiatestnet.com/pools/.
- Delegate again to the new pool. The wallet spends your old delegation record and writes a new one; the staking output itself stays where it is.
- Once it has confirmed, submit the txid. Check the board: your weight should have moved.

The check is automatic: the transaction must spend your delegation record and create one naming another pool on the board.`,
	},
	{
		slug: "unbond-stake", title: "Unbond your stake", category: "Staking", surface: "core", check: "unbond-stake",
		reward: 25, cap: 500, needsTxid: true, sort: 90,
		body: `Start withdrawing your stake.

A stake leaves in two steps: first it moves into an unbonding output of the same key, where it stops counting as weight, and only after the parent chain has moved on far enough can the coins be spent freely. Try it.

- Once your stake's lock has passed, withdraw it from the Staking tab in Sequentia Core; from a node, withdrawstake does the same. If the wallet refuses or the lock has not passed, note the exact message: unclear errors here are worth a bug report.
- Once the unbonding transaction has confirmed, submit its txid.

The check is automatic: the transaction must spend the staking output of your stake task into an unbonding output of the same key.`,
	},
}

type seedComp struct {
	slug, title, body, prizes string
	closesInDays              int
}

var seedComps = []seedComp{
	{
		slug:         "artwork-2026",
		title:        "Sequentia community artwork",
		prizes:       "2000,750,250",
		closesInDays: 28,
		body: `Design a piece of artwork that captures what Sequentia is: a Bitcoin sidechain where every asset has equal standing, anchored to Bitcoin block by block.

What to submit

- One image (PNG or SVG preferred), plus optional variants.
- Host it anywhere public (an image host, a git repository, a portfolio page) and paste the link in your entry.
- Put your account code on the work or its page: a small caption in the image, the file name, or the page's description. An entry that does not carry the entrant's code is not judged.
- Original work only. You keep your copyright and grant Sequentia a license to use the artwork in community material.

Judging

Entries are judged by the Sequentia team on clarity, originality, and how well they reflect the project. Prizes go to first, second, and third place. Winners are announced on this page after the closing date.`,
	},
}

func seedDB(db *sql.DB) {
	var n int64
	if err := db.QueryRow("SELECT COUNT(*) FROM tasks").Scan(&n); err != nil {
		log.Fatalf("seed: %v", err)
	}
	if n == 0 {
		for _, t := range seedTasks {
			_, err := db.Exec(`INSERT INTO tasks (slug, title, category, body, surface, check_kind, reward, cap, needs_txid, active, sort)
				VALUES (?,?,?,?,?,?,?,?,?,1,?)`,
				t.slug, t.title, t.category, t.body, t.surface, t.check, t.reward, t.cap, t.needsTxid, t.sort)
			if err != nil {
				log.Fatalf("seed task %s: %v", t.slug, err)
			}
		}
		log.Printf("seeded %d tasks", len(seedTasks))
	}
	// A database from before tasks named their surface and check gets them
	// for the seeded tasks it holds; copy, rewards and caps stay as they are.
	for _, t := range seedTasks {
		db.Exec("UPDATE tasks SET check_kind = ? WHERE slug = ? AND check_kind = ''", t.check, t.slug)
		db.Exec("UPDATE tasks SET surface = ? WHERE slug = ? AND surface = ''", t.surface, t.slug)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM competitions").Scan(&n); err != nil {
		log.Fatalf("seed: %v", err)
	}
	if n == 0 {
		for _, c := range seedComps {
			closes := time.Now().AddDate(0, 0, c.closesInDays).Unix()
			_, err := db.Exec("INSERT INTO competitions (slug, title, body, prizes, closes_at, status) VALUES (?,?,?,?,?,'open')",
				c.slug, c.title, c.body, c.prizes, closes)
			if err != nil {
				log.Fatalf("seed competition %s: %v", c.slug, err)
			}
		}
		log.Printf("seeded %d competitions", len(seedComps))
	}
}
