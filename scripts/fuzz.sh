#!/usr/bin/env bash
# Runs every Fuzz* target in the module for $FUZZTIME each (default 30s).
#
# A failing input is written under the package's testdata/fuzz/<Target>/.
# Commit it together with the fix: plain `go test` replays every file there,
# so it stays a regression test without any fuzzing.
set -euo pipefail
GO=${GO:-go}
FUZZTIME=${FUZZTIME:-30s}
cd "$(dirname "$0")/.."

fail=0
while IFS=: read -r file decl; do
	name=$(sed -E 's/^func (Fuzz[A-Za-z0-9_]*)\(.*/\1/' <<<"$decl")
	pkg=./$(dirname "$file")
	echo "== $pkg $name ($FUZZTIME)"
	if ! "$GO" test -run '^$' -fuzz "^${name}\$" -fuzztime "$FUZZTIME" "$pkg"; then
		echo "!! $pkg $name failed"
		fail=1
	fi
done < <(grep -rE --include='*_test.go' '^func Fuzz[A-Za-z0-9_]*\(' cmd internal test 2>/dev/null | sort)
exit $fail
