#!/usr/bin/env bash
source "$(dirname "$0")/_helper.sh"

# read_key decodes arrow keys and simple keys from stdin bytes.
t_eq "up"    "$(printf '\033[A' | { read_key; })"    "read_key decodes up arrow"
t_eq "down"  "$(printf '\033[B' | { read_key; })"    "read_key decodes down arrow"
t_eq "right" "$(printf '\033[C' | { read_key; })"    "read_key decodes right arrow"
t_eq "left"  "$(printf '\033[D' | { read_key; })"    "read_key decodes left arrow"
t_eq "enter" "$(printf '\n' | { read_key; })"        "read_key decodes enter"
t_eq "esc"   "$(printf '\033' | { read_key; })"      "read_key decodes a bare escape"
t_eq "q"     "$(printf 'q' | { read_key; })"          "read_key decodes q"
t_eq "space" "$(printf ' ' | { read_key; })"          "read_key decodes space"

# Timeout form: empty stdin -> "tick"
t_eq "tick" "$(read_key 1 < /dev/null)" "read_key with a wait and no input ticks"
