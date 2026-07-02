#!/usr/bin/env python3
"""feed-diff — diff Go GHIVEDIFF FEED dumps against hived ground truth at a block.

Usage:
  feed-diff.py --go-log <wall-run.log> --block <N> \
      [--hived-feed <get_feed_history.json>] [--hived-sched <get_witness_schedule.json>]

With only --go-log: prints Go's reconstructed state at --block (84-sample history,
cmh, schedule) for eyeballing. With the hived JSONs (database_api.get_feed_history /
get_witness_schedule at the same head), prints:
  - per-sample history diff (index, Go base/quote vs hived base/quote), first divergence
  - current_median_history diff
  - schedule set diff (witnesses only in Go / only in hived)

Go log lines parsed (stderr of a replay with GHIVEDIFF_FEED=1 VSDUMP_FROM/TO):
  GHIVEDIFF FEED-MEDIAN block=B median_base=X median_quote=Y     (hourly new sample)
  GHIVEDIFF FEED-HIST   block=B hist_len=L cmh_base=X cmh_quote=Y
  GHIVEDIFF FEED-SCHED  block=B feeds=N schedule=[a,b,...]
  GHIVEDIFF SCHED-VOTES block=B top25=[name=votes,...]
  GHIVEDIFF SCHED-VSTS  block=B top30=[name=vsthex,...]
"""
import argparse
import json
import re
import sys

WINDOW = 84  # HF16+ feed_history window (7d of hourly samples)


def parse_go_log(path):
    medians, hists, scheds, votes, vsts = [], [], [], [], []
    pat = {
        'median': re.compile(r'GHIVEDIFF FEED-MEDIAN block=(\d+) median_base=(\d+) median_quote=(\d+)'),
        'hist': re.compile(r'GHIVEDIFF FEED-HIST block=(\d+) hist_len=(\d+) cmh_base=(\d+) cmh_quote=(\d+)'),
        'sched': re.compile(r'GHIVEDIFF FEED-SCHED block=(\d+) feeds=(\d+) schedule=\[([^\]]*)\]'),
        'votes': re.compile(r'GHIVEDIFF SCHED-VOTES block=(\d+) top25=\[([^\]]*)\]'),
        'vsts': re.compile(r'GHIVEDIFF SCHED-VSTS block=(\d+) top30=\[([^\]]*)\]'),
    }
    with open(path, errors='replace') as f:
        for line in f:
            m = pat['median'].search(line)
            if m:
                medians.append((int(m[1]), int(m[2]), int(m[3])))
                continue
            m = pat['hist'].search(line)
            if m:
                hists.append((int(m[1]), int(m[2]), int(m[3]), int(m[4])))
                continue
            m = pat['sched'].search(line)
            if m:
                scheds.append((int(m[1]), int(m[2]), [w for w in m[3].split(',') if w]))
                continue
            m = pat['votes'].search(line)
            if m:
                votes.append((int(m[1]), m[2]))
                continue
            m = pat['vsts'].search(line)
            if m:
                vsts.append((int(m[1]), m[2]))
    return medians, hists, scheds, votes, vsts


def last_at_or_before(rows, block):
    prev = None
    for r in rows:
        if r[0] > block:
            break
        prev = r
    return prev


def hived_price(p):
    return int(p['base']['amount']), int(p['quote']['amount'])


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('--go-log', required=True)
    ap.add_argument('--block', type=int, required=True)
    ap.add_argument('--hived-feed')
    ap.add_argument('--hived-sched')
    args = ap.parse_args()

    medians, hists, scheds, votes, vsts = parse_go_log(args.go_log)
    if not medians:
        sys.exit('no FEED-MEDIAN lines found — was the run started with GHIVEDIFF_FEED=1 VSDUMP_FROM/TO covering the window?')

    upto = [m for m in medians if m[0] <= args.block]
    go_hist = upto[-WINDOW:]
    go_hist_prices = [(b, q) for _, b, q in go_hist]
    gh = last_at_or_before(hists, args.block)
    gs = last_at_or_before(scheds, args.block)

    print(f'Go @ {args.block}: {len(go_hist)} reconstructed samples '
          f'(hourly medians {go_hist[0][0]}..{go_hist[-1][0]})')
    if gh:
        print(f'Go FEED-HIST: block={gh[0]} hist_len={gh[1]} cmh={gh[2]}/{gh[3]}')
    if gs:
        print(f'Go FEED-SCHED: block={gs[0]} feeds={gs[1]} sched_n={len(gs[2])}')

    if args.hived_feed:
        doc = json.load(open(args.hived_feed))
        res = doc.get('result', doc)
        h_hist = [hived_price(p) for p in res['price_history']]
        h_cmh = hived_price(res['current_median_history'])
        print(f'\nhived: {len(h_hist)} samples, cmh={h_cmh[0]}/{h_cmh[1]}')
        if gh:
            match = 'MATCH' if (gh[2], gh[3]) == h_cmh else 'DIVERGED'
            print(f'cmh: Go {gh[2]}/{gh[3]} vs hived {h_cmh[0]}/{h_cmh[1]} → {match}')
        # Align tails: hived price_history is oldest→newest; compare the
        # overlapping tail sample-by-sample.
        n = min(len(go_hist_prices), len(h_hist))
        g_tail, h_tail = go_hist_prices[-n:], h_hist[-n:]
        g_blocks = [b for b, _, _ in go_hist][-n:]
        first_div = None
        for i in range(n):
            if g_tail[i] != h_tail[i]:
                if first_div is None:
                    first_div = i
                print(f'  sample {i} (Go hour-block {g_blocks[i]}): '
                      f'Go {g_tail[i][0]}/{g_tail[i][1]} vs hived {h_tail[i][0]}/{h_tail[i][1]}')
        if first_div is None:
            print(f'all {n} overlapping samples MATCH')
        else:
            print(f'FIRST DIVERGENT SAMPLE: index {first_div} '
                  f'(Go hour-block {g_blocks[first_div]}) — pull FEED-SCHED/SCHED-VOTES '
                  f'at that block to see the schedule that produced it')

    if args.hived_sched:
        doc = json.load(open(args.hived_sched))
        res = doc.get('result', doc)
        h_sched = sorted(w for w in res['current_shuffled_witnesses'] if w)
        if gs:
            g_set, h_set = set(gs[2]), set(h_sched)
            print(f'\nschedule diff @ Go block {gs[0]} vs hived head:')
            print(f'  only in Go   : {sorted(g_set - h_set) or "-"}')
            print(f'  only in hived: {sorted(h_set - g_set) or "-"}')
            if g_set == h_set:
                print('  schedule sets IDENTICAL')
        # Context lines for the drill.
        gv = last_at_or_before(votes, args.block)
        if gv:
            print(f'\nGo SCHED-VOTES @ {gv[0]}: {gv[1]}')
        gt = last_at_or_before(vsts, args.block)
        if gt:
            print(f'Go SCHED-VSTS @ {gt[0]}: {gt[1]}')


if __name__ == '__main__':
    main()
