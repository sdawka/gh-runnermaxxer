#!/usr/bin/env bash
source "$(dirname "$0")/_helper.sh"

# normalize_url
t_eq "https://github.com/owner/repo" "$(normalize_url "https://github.com/owner/repo/")" "normalize_url strips trailing slash"
t_eq "https://github.com/owner/repo" "$(normalize_url "https://github.com/owner/repo.git")" "normalize_url strips .git suffix"
t_eq "https://github.com/owner/repo" "$(normalize_url "https://github.com/owner/repo.git/")" "normalize_url strips .git then trailing slash"
t_eq "https://github.com/owner/repo" "$(normalize_url "https://github.com/owner/repo")" "normalize_url is a no-op on a clean url"
t_eq "https://github.com/owner/repo" "$(normalize_url "git@github.com:owner/repo.git")" "normalize_url maps an SSH clone URL"
t_eq "https://github.com/owner/repo" "$(normalize_url "git@github.com:owner/repo")" "normalize_url maps an SSH URL without .git"
t_eq "https://github.com/owner/repo" "$(normalize_url "ssh://git@github.com/owner/repo.git")" "normalize_url maps an ssh:// URL"
t_eq "https://github.com/owner/repo" "$(target_entry_to_url "git@github.com:owner/repo.git")" "target_entry_to_url accepts an SSH clone URL"
t_eq "https://github.com/owner/repo" "$(target_entry_to_url "ssh://git@github.com/owner/repo")" "target_entry_to_url accepts an ssh:// URL"

# target_entry_to_url
t_eq "https://github.com/owner/repo" "$(target_entry_to_url "owner/repo")" "target_entry_to_url expands owner/repo"
t_eq "https://github.com/myorg" "$(target_entry_to_url "myorg")" "target_entry_to_url expands bare org name"
t_eq "https://github.com/owner/repo" "$(target_entry_to_url "https://github.com/owner/repo")" "target_entry_to_url leaves a full https url"
t_eq "https://github.com/owner/repo" "$(target_entry_to_url "http://github.com/owner/repo")" "target_entry_to_url upgrades http to https"
t_eq "https://github.com/owner/repo" "$(target_entry_to_url "github.com/owner/repo")" "target_entry_to_url adds scheme to bare github.com/... "
t_eq "" "$(target_entry_to_url "")" "target_entry_to_url of blank line is empty"
t_eq "" "$(target_entry_to_url "# a comment")" "target_entry_to_url of comment line is empty"
t_eq "https://github.com/owner/repo" "$(target_entry_to_url "  owner/repo  ")" "target_entry_to_url trims whitespace"

# valid_repo_url / valid_org_url
t_ok "valid_repo_url accepts owner/repo" valid_repo_url "https://github.com/owner/repo"
t_fail_ok "valid_repo_url rejects bare org" valid_repo_url "https://github.com/owner"
t_ok "valid_org_url accepts bare org" valid_org_url "https://github.com/owner"
t_fail_ok "valid_org_url rejects owner/repo" valid_org_url "https://github.com/owner/repo"

# target_type
t_eq "repo" "$(target_type "https://github.com/owner/repo")" "target_type of repo url"
t_eq "org" "$(target_type "https://github.com/owner")" "target_type of org url"

# target_label
t_eq "owner/repo" "$(target_label "https://github.com/owner/repo")" "target_label of repo url"
t_eq "myorg (organization)" "$(target_label "https://github.com/myorg")" "target_label of org url"

# same_target: case-insensitive
t_ok "same_target is case-insensitive" same_target "https://github.com/Owner/Repo" "https://github.com/owner/repo"
t_fail_ok "same_target rejects a different target" same_target "https://github.com/owner/repo" "https://github.com/owner/other"
t_fail_ok "same_target rejects empty first arg" same_target "" "https://github.com/owner/repo"

# target_api_endpoint
t_eq "repos/owner/repo/actions/runners" "$(target_api_endpoint "https://github.com/owner/repo")" "target_api_endpoint for repo"
t_eq "orgs/myorg/actions/runners" "$(target_api_endpoint "https://github.com/myorg")" "target_api_endpoint for org"
