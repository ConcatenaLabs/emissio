#!/usr/bin/env python3
# A stand-in for the arca command line, answering in its shapes from files
# under its --datadir: requests it made, payments it sent, the mailbox the
# test fills, coin records, Bitcoin ark receipts.
import json, os, sys, binascii
args = sys.argv[1:]
d = args[args.index("--datadir") + 1]
args = args[args.index("--datadir") + 2:]
def out(v): print(json.dumps(v)); sys.exit(0)
def path(*p): return os.path.join(d, *p)
def load(name, default):
    try: return json.load(open(path(name)))
    except Exception: return default
if args[0] == "receive":
    n = load("n.json", 0) + 1; json.dump(n, open(path("n.json"), "w"))
    det = {"kind": "request", "operator": "aa" * 32, "owner": "%064x" % (1000 + n), "owner_nonce": "%064x" % (2000 + n), "until": 9999999999}
    if "--asset" in args: det["asset"] = args[args.index("--asset") + 1]; det["value"] = args[args.index("--amount") + 1]
    out({"request": "arca:" + binascii.hexlify(json.dumps(det).encode()).decode(), "details": det})
if args[0] == "address": out({"address": "tb1qemissioemissioemissioemissioemissio0"})
if args[0] == "info": out({"operator": "aa" * 32})
if args[0] == "balance": out({"arca": {"ab" * 32: {"live": "500000000"}}})
if args[0] == "send":
    sends = load("sends.json", []); sends.append({"request": args[1], "amount": args[args.index("--amount") + 1], "asset": args[args.index("--asset") + 1]})
    json.dump(sends, open(path("sends.json"), "w"))
    out({"sent": {}, "transfer": {"transfer_id": "t%d" % len(sends), "outputs": ["l"]}, "route": "leaf"})
if args[0] == "sync":
    mb = load("mailbox.json", [])
    if os.path.exists(path("mailbox.json")): os.remove(path("mailbox.json"))
    out({"mailbox": {"accepted": mb}, "schedule": {"now": 100, "next_sync_at": 160}})
if args[0] == "record": out({"leaf_id": args[1], "record": load("rec-" + args[1] + ".json", "00")})
if args[0] == "lightning" and args[1] == "pay": out({"side": "bitcoin", "invoice": args[2], "payment": {"state": "paid", "preimage": "cd" * 32}})
if args[0] == "bitcoin":
    rest = [a for a in args[1:] if a != "--quiet"]
    if rest == ["address"]: print("tark1qaccountaddress%d" % (load("ark.json", 0))); sys.exit(0)
    if rest[0] == "history": out([])
    if rest[:2] == ["address", "lookup"]: out(load("ark-" + rest[-1] + ".json", []))
print(json.dumps({"error": {"kind": "refused", "message": "refused: unknown command " + " ".join(args)}})); sys.exit(1)
