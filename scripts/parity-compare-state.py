#!/usr/bin/env python3
"""Compare gopherd's state-jsonl dump against a hived node's database_api.

usage: compare_state.py GO_JSONL HIVED_URL [--max-report N]

Compares, field by field, the objects both sides expose: the dynamic global
properties, the feed history median, every witness (votes, missed blocks,
props, signing key), every account's balances / vesting / withdrawal schedule /
proxy / witness vote count, and the limit orders, HBD conversion requests,
savings withdrawals and pending delegation returns. Manabar fields are skipped
(hived reports pre-HF20 voting power through post-HF20 fields and the two
encodings are not comparable). Needs a MAINNET hived >= 1.27 (the API names
and the compressed block_log); see parity-replay-compare.sh for the whole
procedure.
"""
import json, sys, urllib.request

go_path, url = sys.argv[1], sys.argv[2]
max_report = 8
if len(sys.argv) > 4 and sys.argv[3] == "--max-report":
    max_report = int(sys.argv[4])

def rpc(method, params):
    req = urllib.request.Request(url, data=json.dumps(
        {"jsonrpc": "2.0", "method": method, "params": params, "id": 1}).encode(),
        headers={"Content-Type": "application/json"})
    r = json.load(urllib.request.urlopen(req, timeout=120))
    if "error" in r:
        raise RuntimeError(f"{method}: {r['error']}")
    return r["result"]

B58 = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"
def stm_to_hex(k):
    """STM<base58(33-byte key + 4-byte ripemd160 checksum)> -> 33-byte hex, as gopherd prints it."""
    if not isinstance(k, str) or not k.startswith("STM"):
        return k
    n = 0
    for ch in k[3:]:
        n = n * 58 + B58.index(ch)
    raw = n.to_bytes(37, "big")
    return raw[:33].hex()

def ts(t):
    """hived ISO-8601 time -> unix seconds, as gopherd prints it."""
    import datetime
    return int(datetime.datetime.strptime(t, "%Y-%m-%dT%H:%M:%S").replace(tzinfo=datetime.timezone.utc).timestamp())

def amt(a):
    if isinstance(a, dict):
        return int(a["amount"])
    return int(str(a).split()[0].replace(".", ""))

go = {"account": {}, "witness": {}, "dgpo": None, "feed_history": None}
with open(go_path) as f:
    for line in f:
        row = json.loads(line)
        t = row.get("_type")
        if t == "account":
            go["account"][row["name"]] = row
        elif t == "witness":
            go["witness"][row["owner"]] = row
        elif t in ("dgpo", "feed_history"):
            go[t] = row
print(f"gopherd dump: {len(go['account'])} accounts, {len(go['witness'])} witnesses, dgpo={'yes' if go['dgpo'] else 'no'}")

mismatches = []
def cmp(kind, name, field, hv, gv):
    if hv != gv:
        mismatches.append((kind, name, field, hv, gv))

# --- DGPO ---
h = rpc("database_api.get_dynamic_global_properties", {})
g = go["dgpo"]
if g:
    cmp("dgpo", "", "head_block_number", h["head_block_number"], g["head_block_number"])
    for f in ("current_supply", "current_hbd_supply", "total_vesting_shares",
              "total_vesting_fund_hive", "total_reward_fund_hive", "virtual_supply",
              "pending_rewarded_vesting_shares", "pending_rewarded_vesting_hive"):
        if f in h:
            cmp("dgpo", "", f, amt(h[f]), g[f])
    for f in ("hbd_interest_rate", "hbd_print_rate", "hbd_start_percent", "hbd_stop_percent",
              "content_reward_percent", "vesting_reward_percent", "available_account_subsidies"):
        if f in h:
            cmp("dgpo", "", f, int(h[f]), g[f])
    if "head_block_id" in h:
        cmp("dgpo", "", "head_block_id", h["head_block_id"], g["head_block_id"])

# --- feed ---
fh = rpc("database_api.get_feed_history", {})
g = go["feed_history"]
if g and fh.get("current_median_history"):
    m = fh["current_median_history"]
    cmp("feed", "", "median_base", amt(m["base"]), g["current_median"]["base_amount"])
    cmp("feed", "", "median_quote", amt(m["quote"]), g["current_median"]["quote_amount"])

# --- witnesses ---
seen_w = set()
start = ""
while True:
    r = rpc("database_api.list_witnesses", {"start": start, "limit": 1000, "order": "by_name"})
    ws = r["witnesses"]
    if not ws:
        break
    for w in ws:
        name = w["owner"]
        if name in seen_w:
            continue
        seen_w.add(name)
        gw = go["witness"].get(name)
        if gw is None:
            mismatches.append(("witness", name, "<missing in gopherd>", "", ""))
            continue
        cmp("witness", name, "votes", int(w["votes"]), gw["total_votes_vests"])
        cmp("witness", name, "total_missed", int(w["total_missed"]), gw["total_missed_blocks"])
        cmp("witness", name, "account_creation_fee", amt(w["props"]["account_creation_fee"]), gw["account_creation_fee"])
        cmp("witness", name, "maximum_block_size", int(w["props"]["maximum_block_size"]), gw["maximum_block_size"])
        cmp("witness", name, "hbd_interest_rate", int(w["props"].get("hbd_interest_rate", w["props"].get("sbd_interest_rate", 0))), gw["hbd_interest_rate"])
        cmp("witness", name, "signing_key", stm_to_hex(w["signing_key"]), gw["signing_key"])
    if len(ws) < 1000:
        break
    start = ws[-1]["owner"]
for name in go["witness"]:
    if name not in seen_w:
        mismatches.append(("witness", name, "<missing in hived>", "", ""))
print(f"hived: {len(seen_w)} witnesses")

# --- accounts ---
seen_a = set()
threshold_zero = []
start = ""
fields = [("balance", "balance"), ("hbd_balance", "hbd_balance"), ("vesting_shares", "vesting_shares"),
          ("delegated_vesting_shares", "delegated_vesting_shares"), ("received_vesting_shares", "received_vesting_shares"),
          ("reward_hive_balance", "reward_hive"), ("reward_hbd_balance", "reward_hbd"), ("reward_vesting_balance", "reward_vests"),
          ("savings_balance", "savings_balance"), ("savings_hbd_balance", "savings_hbd_balance"),
          ("vesting_withdraw_rate", "vesting_withdraw_rate"), ("to_withdraw", "to_withdraw"), ("withdrawn", "withdrawn")]
while True:
    r = rpc("database_api.list_accounts", {"start": start, "limit": 1000, "order": "by_name"})
    accts = r["accounts"]
    if not accts:
        break
    for a in accts:
        name = a["name"]
        if name in seen_a:
            continue
        seen_a.add(name)
        ga = go["account"].get(name)
        if ga is None:
            mismatches.append(("account", name, "<missing in gopherd>", "", ""))
            continue
        for hf, gf in fields:
            if hf in a:
                cmp("account", name, gf, amt(a[hf]) if isinstance(a[hf], (dict, str)) else int(a[hf]), ga[gf])
        cmp("account", name, "proxy", a.get("proxy", ""), ga["proxy"])
        cmp("account", name, "witnesses_voted_for", int(a.get("witnesses_voted_for", 0)), ga["witnesses_voted_for"])
        # hived's API "created" is account_object::block_created (the block's own
        # timestamp); gopherd stores account_object::created (head_block_time at
        # apply, one block earlier), which is the value the manabars are seeded
        # from. Neither is read by consensus, so "created" is not compared.
        for hf, gf, conv in (("recovery_account", "recovery_account", str),
                             # hived encodes "no withdrawal" as time_point_sec::maximum, gopherd as 0
                             ("next_vesting_withdrawal", "next_vesting_withdrawal", lambda t: 0 if t.startswith("1969") else ts(t)),
                             ("pending_claimed_accounts", "pending_claimed_accounts", int),
                             ("mined", "mined", bool), ("can_vote", "can_vote", bool)):
            if hf in a and gf in ga:
                cmp("account", name, gf, conv(a[hf]), ga[gf])
        # A13 of the parity review: does any mainnet authority have weight_threshold 0?
        for role in ("owner", "active", "posting"):
            if int(a.get(role, {}).get("weight_threshold", 1)) == 0:
                threshold_zero.append(f"{name}.{role}")
    if len(accts) < 1000:
        break
    start = accts[-1]["name"]
for name in go["account"]:
    if name not in seen_a:
        mismatches.append(("account", name, "<missing in hived>", "", ""))
print(f"hived: {len(seen_a)} accounts; authorities with weight_threshold 0: {len(threshold_zero)} {threshold_zero[:10]}")

# --- auxiliary objects: limit orders, convert requests, savings withdrawals ---
def paged(method, order, start, key):
    out, st = [], start
    while True:
        try:
            r = rpc(method, {"start": st, "limit": 1000, "order": order})
        except Exception as e:
            print(f"  ({method} unavailable: {e})"); return None
        rows = r[key]
        if not rows:
            return out
        out.extend(rows)
        if len(rows) < 1000:
            return out
        st = None
        return out  # one page is enough for the 8M-block counts (< 1000 each)

go_lo = [r for r in map(json.loads, open(go_path)) if r.get("_type") == "limit_order"]
h_lo = paged("database_api.list_limit_orders", "by_account", ["", 0], "orders")
if h_lo is not None:
    gm = {(r["owner"], r["order_id"]): r for r in go_lo}
    hm = {(o["seller"], int(o["orderid"])): o for o in h_lo}
    cmp("limit_order", "", "count", len(hm), len(gm))
    for k, o in hm.items():
        g = gm.get(k)
        if g is None:
            mismatches.append(("limit_order", f"{k[0]}/{k[1]}", "<missing in gopherd>", "", "")); continue
        cmp("limit_order", f"{k[0]}/{k[1]}", "for_sale", int(o["for_sale"]), g["for_sale"])
        cmp("limit_order", f"{k[0]}/{k[1]}", "expiration_set", 1, 1 if g["expiration"] else 0)
    for k in gm:
        if k not in hm:
            mismatches.append(("limit_order", f"{k[0]}/{k[1]}", "<missing in hived>", "", ""))

go_cr = [r for r in map(json.loads, open(go_path)) if r.get("_type") == "convert_request"]
h_cr = paged("database_api.list_hbd_conversion_requests", "by_account", ["", 0], "requests")
if h_cr is not None:
    gm = {(r["owner"], r["request_id"]): r for r in go_cr}
    hm = {(o["owner"], int(o["requestid"])): o for o in h_cr}
    cmp("convert_request", "", "count", len(hm), len(gm))
    for k, o in hm.items():
        g = gm.get(k)
        if g is None:
            mismatches.append(("convert_request", f"{k[0]}/{k[1]}", "<missing in gopherd>", "", "")); continue
        cmp("convert_request", f"{k[0]}/{k[1]}", "amount", amt(o["amount"]), g["amount"])

go_sw = [r for r in map(json.loads, open(go_path)) if r.get("_type") == "savings_withdraw"]
h_sw = paged("database_api.list_savings_withdrawals", "by_from_id", ["", 0], "withdrawals")
if h_sw is not None:
    gm = {(r["from"], r["request_id"]): r for r in go_sw}
    hm = {(o["from"], int(o["request_id"])): o for o in h_sw}
    cmp("savings_withdraw", "", "count", len(hm), len(gm))
    for k, o in hm.items():
        g = gm.get(k)
        if g is None:
            mismatches.append(("savings_withdraw", f"{k[0]}/{k[1]}", "<missing in gopherd>", "", "")); continue
        cmp("savings_withdraw", f"{k[0]}/{k[1]}", "amount", amt(o["amount"]), g["amount"])
        cmp("savings_withdraw", f"{k[0]}/{k[1]}", "to", o["to"], g["to"])

go_vde = sum(1 for r in map(json.loads, open(go_path)) if r.get("_type") == "vesting_delegation_expiration")
h_vde = paged("database_api.list_vesting_delegation_expirations", "by_account_expiration", ["", "1970-01-01T00:00:00", 0], "delegations")
if h_vde is not None:
    cmp("vesting_delegation_expiration", "", "count", len(h_vde), go_vde)

by_field = {}
for kind, name, field, hv, gv in mismatches:
    by_field.setdefault(f"{kind}.{field}", []).append((name, hv, gv))
print(f"\nMISMATCHES: {len(mismatches)}")
for k, v in sorted(by_field.items(), key=lambda kv: -len(kv[1])):
    print(f"  {k}: {len(v)}")
print()
# a few examples per field, so one systematic difference does not hide the rest
shown = {}
for kind, name, field, hv, gv in mismatches:
    k = f"{kind}.{field}"
    if shown.get(k, 0) < max_report:
        shown[k] = shown.get(k, 0) + 1
        print(f"  {kind} {name} {field}: hived={hv} gopherd={gv}")
sys.exit(1 if mismatches else 0)
