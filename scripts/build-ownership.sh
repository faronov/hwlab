#!/bin/sh
set -eu

action=$1
dir=$2
shift 2
manifest="$dir/.hwlab-build-ownership"

case "$action" in
record)
    tmp=$(mktemp "$dir/.hwlab-build-ownership-XXXXXX")
    trap 'rm -f -- "$tmp"' EXIT HUP INT TERM
    for name do
        case "$name" in ""|.|..|*/*) echo "invalid build output name: $name" >&2; exit 1;; esac
        file="$dir/$name"
        if [ -L "$file" ] || [ ! -f "$file" ]; then
            echo "not a regular build output: $file" >&2
            exit 1
        fi
        digest=$(sha256sum -- "$file")
        printf '%s %s\n' "${digest%% *}" "$name" >> "$tmp"
    done
    mv -- "$tmp" "$manifest"
    ;;
clean)
    if [ ! -f "$manifest" ] || [ -L "$manifest" ]; then
        echo "No ownership manifest; leaving $dir untouched" >&2
        exit 0
    fi
    while IFS=' ' read -r expected name; do
        case "$name" in ""|.|..|*/*) echo "invalid ownership entry" >&2; exit 1;; esac
        file="$dir/$name"
        if [ -L "$file" ]; then
            echo "Keeping replaced build output: $file" >&2
        elif [ -f "$file" ]; then
            actual=$(sha256sum -- "$file")
            if [ "${actual%% *}" = "$expected" ]; then
                rm -- "$file"
            else
                echo "Keeping modified build output: $file" >&2
            fi
        fi
    done < "$manifest"
    rm -- "$manifest"
    # A directory containing user/reference files is deliberately retained.
    if [ -d "$dir" ]; then
        rmdir -- "$dir" 2>/dev/null || :
    fi
    ;;
*)
    echo "usage: $0 record|clean DIR [created output names...]" >&2
    exit 1
    ;;
esac
