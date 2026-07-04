package main

import (
	"database/sql"
	"log"
	"time"
)

// Program constants. Amounts are whole SEQ.
const (
	programPool    int64   = 1_000_000
	launchPriceUSD float64 = 0.375
)

// Security reward tiers (whole SEQ), shown on the security page and used as
// defaults in the admin review form.
var securityTiers = []struct {
	Name   string
	Reward int64
	Desc   string
}{
	{"Low", 250, "Minor issues with limited impact: UI bugs with a security angle, hard-to-trigger crashes, documentation errors that could mislead users into unsafe behavior."},
	{"Medium", 1_000, "Real but contained vulnerabilities: denial of service against a single node, RPC weaknesses, wallet bugs that could lose testnet funds under unusual conditions."},
	{"High", 4_000, "Serious vulnerabilities: remote crash of many nodes, theft of funds requiring user interaction, breaking the opt-in confidentiality of a blinded transaction."},
	{"Critical", 13_500, "Network-level vulnerabilities: consensus splits, silent inflation of any asset, theft of funds without user interaction. Consensus-breaking or funds-loss bugs with a working proof of concept can be awarded up to 27,000 SEQ."},
}

type seedTask struct {
	slug, title, category, body string
	reward, cap                 int64
	needsTxid                   bool
	sort                        int64
}

var seedTasks = []seedTask{
	{
		slug: "first-transaction", title: "Make your first testnet transaction", category: "Getting started",
		reward: 10, cap: 5000, needsTxid: true, sort: 10,
		body: `Create a Sequentia testnet wallet, claim tSEQ from the faucet, and send some of it to any address.

Any Sequentia wallet works: the web wallet at sequentiatestnet.com/wallet, the Ambra mobile wallet, or your own node.

- Create a wallet and copy your testnet address (it starts with tb1).
- Claim tSEQ from the faucet at sequentiatestnet.com.
- Send any amount to another address. Sending to a second wallet of your own is fine.
- Paste your claim code (shown on your Emissio account page) into the notes field below, and submit the txid of the send.

We check that the transaction exists on the testnet and pays a fee from your address.`,
	},
	{
		slug: "issue-asset", title: "Issue your own asset", category: "Assets",
		reward: 20, cap: 2500, needsTxid: true, sort: 20,
		body: `Issue a new asset on the Sequentia testnet.

On Sequentia, anyone can issue an asset, and every issued asset has equal standing on the chain. Issue one of your own: a point system, a voucher, a test stablecoin, anything.

- Issue the asset from your wallet or node (for example with the issueasset RPC on your own node).
- Submit the issuance txid.
- In the notes, tell us the asset ID and, if you like, what the asset is meant to be.`,
	},
	{
		slug: "any-asset-fee", title: "Pay a fee in an asset other than tSEQ", category: "Assets",
		reward: 15, cap: 2500, needsTxid: true, sort: 30,
		body: `Send a transaction whose fee is paid in an issued asset instead of tSEQ.

Sequentia has an open fee market: fees can be paid in any accepted asset, and tSEQ has no special status beyond staking. Get some GOLD, USDX or another asset from the faucet or the DEX, then send a transaction and choose that asset as the fee asset.

- Submit the txid of the transaction.
- We check on-chain that the fee output is in a non-tSEQ asset.`,
	},
	{
		slug: "confidential-tx", title: "Send an opt-in confidential transaction", category: "Assets",
		reward: 15, cap: 2000, needsTxid: true, sort: 40,
		body: `Sequentia is transparent by default, and confidentiality is opt-in. Generate a confidential address (it starts with tsqb) and receive funds to it, so the amounts are blinded on-chain.

- Generate a blinded address, for example with getnewaddress "" blech32 on your node.
- Send funds from your transparent address to the blinded address.
- Submit the txid. We check that the transaction has blinded outputs.`,
	},
	{
		slug: "seqdex-swap", title: "Complete a SeqDEX swap", category: "Trading",
		reward: 25, cap: 1500, needsTxid: true, sort: 50,
		body: `Complete an atomic swap between two assets on SeqDEX, the order-book DEX.

Take an existing offer from the book, or post your own and wait for it to fill. Both legs of the swap settle in a single atomic transaction on the Sequentia testnet.

- Submit the txid of the settlement transaction.
- In the notes, mention which market you traded (for example GOLD/tSEQ).`,
	},
	{
		slug: "cross-chain-swap", title: "Complete a cross-chain swap with Bitcoin testnet", category: "Trading",
		reward: 40, cap: 750, needsTxid: true, sort: 60,
		body: `Complete a cross-chain swap between Bitcoin testnet4 BTC and a Sequentia testnet asset.

Every Sequentia block is anchored to a Bitcoin block, which is what makes these swaps safe without a custodian: if Bitcoin reorganizes, Sequentia follows in real time.

- Use the cross-chain markets on SeqDEX.
- Submit the txid of the Sequentia leg of your swap.
- In the notes, include the Bitcoin testnet4 txid of the other leg.`,
	},
	{
		slug: "lightning-swap", title: "Complete a Lightning swap", category: "Trading",
		reward: 40, cap: 750, needsTxid: true, sort: 70,
		body: `Complete a swap over Lightning on the Sequentia testnet: a submarine swap between an on-chain asset and Lightning BTC, or a pure Lightning swap if you run SeqLN.

- Submit the txid of the on-chain leg (for a submarine swap) or of your channel funding transaction (for a pure Lightning swap).
- In the notes, include the payment hash of the Lightning leg.`,
	},
	{
		slug: "run-node", title: "Run a full node for a week", category: "Infrastructure",
		reward: 50, cap: 1000, needsTxid: true, sort: 80,
		body: `Run a Sequentia testnet full node continuously for seven days.

Full-node sovereignty is a core Sequentia principle: block producers cannot force rule changes on nodes that validate everything themselves.

- Sync a full node from sequentiatestnet.com downloads or by building from source.
- After seven days of uptime, send a transaction from the node's wallet with your claim code in the notes field of this submission, and submit that txid.
- In the notes, include your node's uptime and the output of getblockchaininfo (blocks and bestblockhash).`,
	},
}

type seedComp struct {
	slug, title, body, prizes string
	closesInDays              int
}

var seedComps = []seedComp{
	{
		slug:  "artwork-2026",
		title: "Sequentia community artwork",
		prizes: "2000,750,250",
		closesInDays: 28,
		body: `Design a piece of artwork that captures what Sequentia is: a Bitcoin sidechain where every asset has equal standing, anchored to Bitcoin block by block.

What to submit

- One image (PNG or SVG preferred), plus optional variants.
- Host it anywhere public (an image host, a git repository, a portfolio page) and paste the link in your entry.
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
