set -o pipefail
find . -type f -print0 | while IFS= read -r -d '' file; do
  while :; do
    output=$(claude \
      --verbose \
      --permission-mode auto \
      --print "Implement full parity with hived's c++ version located at @../hive. \
              your goal is to implement the full c++ tests and their respective evals that are matching to have 100% test parity. Finally you must be able to fully replay the block_log. ALWAYS do full implementations of features, never do a shortcut that is 'good enough for now' always implement the full feature just like it is in the c++ version. " 2>&1 | tee /dev/stderr)
    status=$?

    if [ $status -eq 0 ] && ! echo "$output" | grep -qiE 'rate limit|usage limit|quota|too many requests|out of tokens|limit reached|try again'; then
      break
    fi

    echo "[aa.sh] claude hit a limit (or failed). Sleeping 5m before retry..." >&2
    sleep 300
  done
done
