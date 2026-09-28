#!/usr/bin/env bash
source "$(dirname "$0")/_helper.sh"

MAX_RUNNERS=5

setup_menu() {
    M_URLS=("https://github.com/a/one" "https://github.com/b/two")
    M_WANT=(2 1)
    M_CUR=(2 1)
    M_SEL=0
    M_MSG=""
}

# menu_total_want sums M_WANT
setup_menu
t_eq "3" "$(menu_total_want)" "menu_total_want sums all rows"

# menu_adjust: cannot go below 0
setup_menu
M_WANT=(0 1)
menu_adjust -1
t_eq "0" "${M_WANT[0]}" "menu_adjust never goes below 0"

# menu_adjust: normal increment
setup_menu
menu_adjust 1
t_eq "3" "${M_WANT[0]}" "menu_adjust increments the selected row"

# menu_adjust: cannot exceed MAX_RUNNERS in total, and sets M_MSG
setup_menu
M_WANT=(4 1)
M_SEL=0
menu_adjust 1
t_eq "4" "${M_WANT[0]}" "menu_adjust refuses an increment that would exceed MAX_RUNNERS"
case "$M_MSG" in
    *"MAX_RUNNERS"*) t_eq "1" "1" "menu_adjust sets M_MSG when MAX_RUNNERS reached" ;;
    *) t_eq "MAX_RUNNERS message" "$M_MSG" "menu_adjust sets M_MSG when MAX_RUNNERS reached" ;;
esac

# menu_adjust: decrement never blocked by MAX_RUNNERS
setup_menu
M_WANT=(5 0)
M_SEL=0
menu_adjust -1
t_eq "4" "${M_WANT[0]}" "menu_adjust allows decrementing even at MAX_RUNNERS"

# menu_set: sets an exact value within budget
setup_menu
M_SEL=1
menu_set 3
t_eq "3" "${M_WANT[1]}" "menu_set applies an exact value"

# menu_set: refuses a value that would push the total over MAX_RUNNERS
setup_menu
M_SEL=0
M_WANT=(2 1)
menu_set 10
t_eq "2" "${M_WANT[0]}" "menu_set refuses a value exceeding MAX_RUNNERS across all rows"
case "$M_MSG" in
    *"MAX_RUNNERS"*) t_eq "1" "1" "menu_set sets M_MSG when refused" ;;
    *) t_eq "MAX_RUNNERS message" "$M_MSG" "menu_set sets M_MSG when refused" ;;
esac

# menu_set: exactly at MAX_RUNNERS is allowed
setup_menu
M_SEL=0
M_WANT=(2 1)
menu_set 4
t_eq "4" "${M_WANT[0]}" "menu_set allows a value that lands exactly at MAX_RUNNERS"

# menu_adjust / menu_set on an empty menu is a harmless no-op
M_URLS=(); M_WANT=(); M_CUR=(); M_SEL=0; M_MSG=""
menu_adjust 1
t_eq "0" "${#M_WANT[@]}" "menu_adjust on an empty menu does nothing"
menu_set 3
t_eq "0" "${#M_WANT[@]}" "menu_set on an empty menu does nothing"
