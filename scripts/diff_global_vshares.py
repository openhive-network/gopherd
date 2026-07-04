#!/usr/bin/env python3
# Diff per-block GHIVEDIFF GLOBAL vshares (and vfund/prvs/prvh) between Go and hived.
# Finds the FIRST block where any pool field diverges.
# Usage: diff_global_vshares.py <go.log> <hived.log>
import sys, re

def load(path):
    d = {}
    pat = re.compile(r'GHIVEDIFF GLOBAL block=(\d+).*?vfund=(\d+) vshares=(\d+).*?prvs=(\d+) prvh=(\d+)')
    with open(path, errors='ignore') as f:
        for line in f:
            if 'GHIVEDIFF GLOBAL' not in line:
                continue
            m = pat.search(line)
            if m:
                b = int(m.group(1))
                d[b] = (int(m.group(2)), int(m.group(3)), int(m.group(4)), int(m.group(5)))
    return d

go = load(sys.argv[1])
hv = load(sys.argv[2])
common = sorted(set(go) & set(hv))
print(f"go blocks={len(go)} hived blocks={len(hv)} common={len(common)} range={common[0] if common else '-'}..{common[-1] if common else '-'}")
first = None
for b in common:
    gf, gv, gp, gh = go[b]
    hf, hv2, hp, hh = hv[b]
    if (gv, gf, gp, gh) != (hv2, hf, hp, hh):
        if first is None:
            first = b
            print(f"FIRST DIVERGENCE at block {b}:")
            print(f"  vshares Go={gv} hived={hv2} Δ={gv-hv2:+d}")
            print(f"  vfund   Go={gf} hived={hf} Δ={gf-hf:+d}")
            print(f"  prvs    Go={gp} hived={hp} Δ={gp-hp:+d}")
            print(f"  prvh    Go={gh} hived={hh} Δ={gh-hh:+d}")
            # print a few blocks before for context
            idx = common.index(b)
            print("  --- 3 blocks before (should match) ---")
            for pb in common[max(0,idx-3):idx]:
                print(f"    blk {pb}: vshares Go={go[pb][1]} hived={hv[pb][1]} Δ={go[pb][1]-hv[pb][1]:+d}")
            break
if first is None:
    print("NO divergence in common range.")
    # show endpoint drift
    b = common[-1]
    print(f"  endpoint blk {b}: vshares Δ={go[b][1]-hv[b][1]:+d} vfund Δ={go[b][0]-hv[b][0]:+d} prvs Δ={go[b][2]-hv[b][2]:+d} prvh Δ={go[b][3]-hv[b][3]:+d}")
