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
	reward, cap                 int64
	needsTxid                   bool
	sort                        int64
}

// retiredTasks are seeded tasks of an earlier catalogue that reseed-tasks
// deactivates: the products they need are not part of the public testnet.
var retiredTasks = []string{"seqdex-swap", "cross-chain-swap", "lightning-swap"}

var seedTasks = []seedTask{
	{
		slug: "first-transaction", title: "Make your first testnet transaction", category: "Getting started",
		reward: 10, cap: 5000, needsTxid: true, sort: 10,
		body: `Install Sequentia Core, claim tSEQ from the faucet, and send an amount that proves the transaction is yours.

- Download Sequentia Core from sequentiatestnet.com/download/core/ (Linux or Windows) and start it. It connects to the testnet on its own; let it sync.
- In the wallet, choose Receive and create an address (it starts with tb1). From a node, sequentia-cli getnewaddress does the same.
- Claim tSEQ at sequentiatestnet.com/faucet/ to that address.
- Send exactly 1.{PROOF} tSEQ to another address. A second address of your own is fine. The eight decimals are your proof decimals: they are how we know the transaction is yours, and every transaction you submit for any task must carry them in one of its outputs.
- Submit the txid of the send. You can find it in the wallet's transaction list, and check it at sequentiatestnet.com/explorer/.`,
	},
	{
		slug: "anchor-lookup", title: "Find the Bitcoin anchor of your transaction", category: "Getting started",
		reward: 10, cap: 3000, needsTxid: true, sort: 15,
		body: `Every Sequentia block commits to a Bitcoin testnet4 block header, and Sequentia reorganises whenever Bitcoin does. Trace that link for one of your own transactions.

- Send a transaction with an output of 1.{PROOF} tSEQ (your first-transaction send qualifies once it has confirmed).
- Open it in the explorer at sequentiatestnet.com/explorer/ and go to the block that contains it. The block page shows the Bitcoin testnet4 block it anchors to.
- Submit the txid, and in the notes paste the anchored Bitcoin block hash and, if you like, its height on testnet4.

The hash in your notes is compared with the anchor recorded for that block. If the explorer makes this hard to find, say so in the notes: that is exactly the kind of feedback the first wave is for.`,
	},
	{
		slug: "issue-asset", title: "Issue your own asset", category: "Assets",
		reward: 20, cap: 2500, needsTxid: true, sort: 20,
		body: `Issue a new asset on the Sequentia testnet.

On Sequentia, anyone can issue an asset, and every issued asset has equal standing on the chain. Issue one of your own: a point system, a voucher, a test stablecoin, anything.

- Issue the asset from the desktop wallet, or with the issueasset RPC on your node, with an amount that ends in your proof decimals: for example 1000.{PROOF}. Ask for a reissuance token as well: the next task uses it.
- Submit the issuance txid. In the notes, tell us the asset ID and, if you like, what the asset is meant to be.

The transaction is checked for a new issuance and for the proof decimals on the issued amount.`,
	},
	{
		slug: "reissue-asset", title: "Reissue an asset you created", category: "Assets",
		reward: 20, cap: 1000, needsTxid: true, sort: 25,
		body: `Increase the supply of an asset you issued, using its reissuance token.

Reissuance is how an issuer mints more of an existing asset rather than creating a new one. It works on transparent assets as it does on confidential ones.

- Take the asset you issued in the previous task; the reissuance is matched against that issuance.
- Reissue an amount ending in your proof decimals, for example 500.{PROOF}, from the desktop wallet or with the reissueasset RPC.
- Submit the reissuance txid.

The transaction is checked for a reissuance of an asset whose issuance you submitted, and for the proof decimals.`,
	},
	{
		slug: "any-asset-fee", title: "Pay a fee in an asset other than tSEQ", category: "Assets",
		reward: 15, cap: 2500, needsTxid: true, sort: 30,
		body: `Send a transaction whose fee is paid in an issued asset instead of tSEQ.

Sequentia has an open fee market: fees can be paid in any accepted asset, and tSEQ has no special status beyond staking. Claim USDX, GOLD or another asset from the faucet, then send a transaction and choose that asset as the fee asset.

- In the desktop wallet the fee asset is a choice on the send screen; from a node, pass fee_asset_label or fee_asset to sendtoaddress.
- Make one output an amount ending in your proof decimals, for example 1.{PROOF} of whatever you send.
- Submit the txid. The fee output's asset and the proof decimals are checked on-chain.`,
	},
	{
		slug: "multi-asset-send", title: "Send two assets in one transaction", category: "Assets",
		reward: 15, cap: 2000, needsTxid: true, sort: 35,
		body: `Send two different assets to another address in a single transaction.

A Sequentia transaction can move several assets at once; a wallet that handles this correctly is one of the things the first wave is meant to test.

- Hold at least two assets, for example tSEQ and USDX from the faucet, or an asset you issued.
- In the desktop wallet, add a second recipient and choose a different asset for it; from a node, sendmany with an assets argument does the same. Make one of the amounts end in your proof decimals, for example 1.{PROOF}.
- Submit the txid. The transaction is checked for outputs in two assets and for the proof decimals.`,
	},
	{
		slug: "confidential-tx", title: "Send an opt-in confidential transaction", category: "Assets",
		reward: 15, cap: 2000, needsTxid: true, sort: 40,
		body: `Sequentia is transparent by default, and confidentiality is opt-in. Generate a confidential address (it starts with tsqb) and receive funds to it, so the amounts are blinded on-chain.

- Generate a blinded address, for example with getnewaddress "" blech32 on your node.
- In one transaction, send any amount to the blinded address and 1.{PROOF} tSEQ to a transparent address of yours (a second recipient in the desktop wallet, or sendmany from a node). The blinded output hides its amount, so the proof decimals must ride on a transparent one.
- Submit the txid. The transaction is checked for a blinded output and for the proof decimals.`,
	},
	{
		slug: "bridge-in", title: "Bridge an asset into Sequentia with Compages", category: "Bridge",
		reward: 30, cap: 1000, needsTxid: true, sort: 50,
		body: `Lock an asset on Ethereum Sepolia or Solana devnet and receive the bridged asset on Sequentia.

Compages, at sequentiatestnet.com/bridge/, locks the original in a vault and mints a Sequentia asset for it; later deposits of the same token reissue the same asset.

- Get some Sepolia ETH, or devnet SOL, from any public faucet for that network.
- On the bridge page, connect your wallet, choose the asset and amount, and give a Sequentia address of yours as the destination. Wait for the deposit to confirm and the bridged asset to be delivered.
- The delivery is made by the bridge, so it cannot carry your proof. Prove control instead: send some of the bridged asset to another address of yours, with an amount ending in your proof decimals (for example 0.{PROOF} if you bridged little).
- Submit the txid of that send. In the notes, give the deposit transaction hash on the source chain and the delivery txid.`,
	},
	{
		slug: "bridge-out", title: "Bridge an asset back out with Compages", category: "Bridge",
		reward: 40, cap: 500, needsTxid: true, sort: 60,
		body: `Return a bridged asset to its original chain.

The vault releases the original only after the return transaction's Bitcoin anchor is buried deep enough that a Bitcoin reorganisation cannot undo it. That wait is deliberate, and this task exercises it end to end.

- On sequentiatestnet.com/bridge/, create a redemption address bound to your Ethereum or Solana address.
- Send the bridged asset to that redemption address from your Sequentia wallet, with an amount ending in your proof decimals, for example 0.{PROOF}.
- Wait for the release on the source chain; the bridge page tracks it.
- Submit the Sequentia txid of your return transaction. In the notes, give the release transaction hash on the source chain and, roughly, how long the release took.`,
	},
	{
		slug: "build-from-source", title: "Build Sequentia Core from source", category: "Infrastructure",
		reward: 30, cap: 500, needsTxid: false, sort: 70,
		body: `Build the node from the source repository on your own machine and prove it by what the build announces to the network.

The download page offers Linux and Windows builds; everything else, and anyone who wants to read what they run, builds from github.com/ConcatenaLabs/Sequentia following its build documentation.

- Before building, open src/clientversion.cpp and change the client name from "Sequentia Core" to "Emissio-{CODE}" (your account code). A release binary can add a comment to its user agent, but only a rebuild can change the name itself, so the name is the proof.
- Build sequentiad and sequentia-cli (the desktop wallet is optional) on your platform.
- Run your build on the testnet with -addnode=159.195.15.140:18444 so it connects to the public node, and submit this task while it is running. No txid is needed.
- In the notes, give your operating system and version, and anything in the build instructions that was wrong or missing.

At submission the public node's peer list is read; a peer announcing /Emissio-{CODE}:... passes.`,
	},
	{
		slug: "stake", title: "Stake tSEQ", category: "Infrastructure",
		reward: 40, cap: 500, needsTxid: true, sort: 75,
		body: `Lock a stake and register as a potential block producer.

Only the Sequence token can stake, and the minimum is 40,000 tSEQ, which one faucet claim covers. A staking output keeps its weight for as long as it is unspent; the lock only gates withdrawal.

- In Sequentia Core, open the Staking tab and create a staking output of 40000.{PROOF} tSEQ (at least 40,000, ending in your proof decimals) with the shortest lock the chain allows. From a node, getstakescript builds the script and you send the stake to it.
- Keep the node running; the Staking tab shows your weight and whether you are registered.
- Submit the txid of the staking output. In the notes, include the output of getstakerinfo.

The output is checked for tSEQ of at least 40,000 with your proof decimals; a reviewer confirms the staking script. With the testnet committee's weight you are unlikely to be elected soon, and that is fine.`,
	},
	{
		slug: "delegate-stake", title: "Delegate your stake to a staking pool", category: "Infrastructure",
		reward: 30, cap: 500, needsTxid: true, sort: 77,
		body: `Lend your stake's block-signing rights to a pool without moving your coins.

A delegation is a separate record on the chain that points your staking output at a pool's signer. The coins never leave your wallet, the pool can never spend them, and you can re-point or withdraw the delegation at any time. Pool payouts are proportional, through an on-chain pot anyone can claim.

- Pick a pool on the board at sequentiatestnet.com/pools/.
- In Sequentia Core's Staking tab, delegate your staking output (see the previous task) to that pool; from a node, delegatestake does the same. If the wallet lets you choose the record's amount, make it end in your proof decimals.
- Submit the txid of the delegation record. In the notes, name the pool and include the output of listdelegations.

A reviewer checks the record against the pool board: it must point a stake of yours, the one from the previous task, at that pool's signer.`,
	},
	{
		slug: "run-node", title: "Run a full node for a week", category: "Infrastructure",
		reward: 50, cap: 1000, needsTxid: false, sort: 80,
		body: `Run a Sequentia testnet full node continuously for seven days, and let the network see it.

Full-node sovereignty is a core Sequentia principle: block producers cannot force rule changes on nodes that validate everything themselves.

- Run a full node, from sequentiatestnet.com/download/core/ or your own build, with two options: -uacomment=emissio-{CODE} (your account code, which the node announces to its peers) and -addnode=159.195.15.140:18444 so it stays connected to the public node.
- Keep it running for a week. The public node records every ten minutes which nodes announcing an Emissio code are connected.
- After seven days, submit this task. No txid is needed; in the notes, include the output of getblockchaininfo (blocks and bestblockhash).

The check counts the ten-minute slots of the last week in which your node was seen; a week's span with at least 80% of slots present passes.`,
	},
	{
		slug: "report-bug", title: "Report a bug", category: "Quality",
		reward: 25, cap: 400, needsTxid: false, sort: 90,
		body: `Find and report a real bug that is not a security vulnerability: a documentation error that would send a user the wrong way, a wallet or explorer screen that shows the wrong thing, a crash or hang you can reproduce, a command in a guide that no longer works.

- File the bug as a public issue in the repository that owns the code, under github.com/ConcatenaLabs, with steps to reproduce, and put your account code {CODE} in the issue text so the issue is known to be yours.
- Paste the issue link in the notes below. The issue is read from GitHub and checked for the code.

The task pays once per account, for the first issue of yours that a maintainer confirms. Further confirmed bugs are welcome on GitHub and count toward competitions when one is running. Anything with a security consequence belongs on the security page instead, where it is reviewed privately and paid by severity.`,
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
			_, err := db.Exec(`INSERT INTO tasks (slug, title, category, body, reward, cap, needs_txid, active, sort)
				VALUES (?,?,?,?,?,?,?,1,?)`,
				t.slug, t.title, t.category, t.body, t.reward, t.cap, t.needsTxid, t.sort)
			if err != nil {
				log.Fatalf("seed task %s: %v", t.slug, err)
			}
		}
		log.Printf("seeded %d tasks", len(seedTasks))
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
