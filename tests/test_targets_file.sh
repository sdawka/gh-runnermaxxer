#!/usr/bin/env bash
source "$(dirname "$0")/_helper.sh"

# add_target_to_file / target_in_file
add_target_to_file "https://github.com/owner/repo"
t_ok "target_in_file finds a just-added target" target_in_file "https://github.com/owner/repo"
t_ok "target_in_file is case-insensitive" target_in_file "https://github.com/OWNER/REPO"
t_fail_ok "target_in_file doesn't find an unlisted target" target_in_file "https://github.com/other/repo"

# add_target_to_file is idempotent (no duplicate lines)
add_target_to_file "https://github.com/owner/repo"
count=$(grep -c '^owner/repo$' "$TARGETS_FILE")
t_eq "1" "$count" "add_target_to_file does not duplicate an existing entry"

# adding a second target appends correctly
add_target_to_file "https://github.com/myorg"
t_ok "target_in_file finds second target" target_in_file "https://github.com/myorg"

# load_targets: skips invalid/comment/blank lines but does NOT itself
# dedupe (that's known_targets' job, tested below) - it just expands and
# validates each line, in file order.
cat > "$TARGETS_FILE" << 'EOF'
# a comment

owner/repo
owner/repo
OWNER/REPO
not a valid target!!
myorg
EOF
loaded=$(load_targets | tr '\n' ',')
t_eq "https://github.com/owner/repo,https://github.com/owner/repo,https://github.com/OWNER/REPO,https://github.com/myorg," "$loaded" "load_targets expands valid lines in order and drops invalid/comment/blank lines"

# known_targets: dedupes case-insensitively (first occurrence wins)
known=$(known_targets | tr '\n' ',')
t_eq "https://github.com/owner/repo,https://github.com/myorg," "$known" "known_targets dedupes case-insensitively"

# remove_target_from_file drops matching entries but keeps comments/blanks
remove_target_from_file "https://github.com/owner/repo"
t_fail_ok "removed target is no longer in file" target_in_file "https://github.com/owner/repo"
t_ok "unrelated target survives removal" target_in_file "https://github.com/myorg"
t_ok "comment line survives removal" grep -q '^# a comment$' "$TARGETS_FILE"

# known_targets with no runner dirs: just the targets-file entries
: > "$TARGETS_FILE"
add_target_to_file "https://github.com/owner/repo"
add_target_to_file "https://github.com/myorg"
known=$(known_targets | tr '\n' ',')
t_eq "https://github.com/owner/repo,https://github.com/myorg," "$known" "known_targets with no runner dirs lists targets-file entries in order"
