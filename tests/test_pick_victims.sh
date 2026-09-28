#!/usr/bin/env bash
source "$(dirname "$0")/_helper.sh"

URL="https://github.com/owner/repo"
make_runner_dir 1 "$URL"
make_runner_dir 2 "$URL"
make_runner_dir 3 "$URL"
make_runner_dir 4 "$URL"
make_runner_dir 5 "https://github.com/owner/other"   # different target - must be excluded

# Stub is_busy: runner-3 is busy, everything else idle.
is_busy() { [[ "$1" == "3" ]]; }

# idle ones first (highest id first within idle), then busy ones
result=$(pick_victims "$URL" 10 | tr '\n' ',')
t_eq "4,2,1,3," "$result" "pick_victims orders idle-highest-first then busy, excluding other targets"

# Limit to N
result=$(pick_victims "$URL" 2 | tr '\n' ',')
t_eq "4,2," "$result" "pick_victims prints at most N ids"

# All busy: still returns them all in highest-id-first order
is_busy() { return 0; }
result=$(pick_victims "$URL" 10 | tr '\n' ',')
t_eq "4,3,2,1," "$result" "pick_victims with everything busy is still highest-id-first"

# No runners for this target
result=$(pick_victims "https://github.com/nobody/here" 5 | tr '\n' ',')
t_eq "" "$result" "pick_victims for a target with no runners is empty"
