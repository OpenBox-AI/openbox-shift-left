#!/bin/sh
set -eu

if [ "$#" -ne 2 ]; then
  echo "usage: publish-candidate.sh TEMPORARY_FILE NEW_CANDIDATE_JSON" >&2
  exit 2
fi

source_path=$1
target_path=$2
source_base=$(basename -- "$source_path")
target_base=$(basename -- "$target_path")
source_parent=$(dirname -- "$source_path")
target_parent=$(dirname -- "$target_path")

case "$source_base" in
  .openbox-security-analysis.tmp.*) ;;
  *) echo "publisher: source is not the owned temporary file" >&2; exit 1 ;;
esac

if [ "$target_base" = "." ] || [ "$target_base" = ".." ] || [ "$target_base" = "/" ]; then
  echo "publisher: invalid target basename" >&2
  exit 1
fi
if [ ! -d "$source_parent" ] || [ ! -d "$target_parent" ]; then
  echo "publisher: source and target parents must exist" >&2
  exit 1
fi

source_parent=$(cd -P -- "$source_parent" && pwd)
target_parent=$(cd -P -- "$target_parent" && pwd)
if [ "$source_parent" != "$target_parent" ]; then
  echo "publisher: temporary file must share the target parent" >&2
  exit 1
fi

source_path=$source_parent/$source_base
target_path=$target_parent/$target_base
if [ -L "$source_path" ] || [ ! -f "$source_path" ]; then
  echo "publisher: source must be a regular non-link file" >&2
  exit 1
fi

if stat -f '%Lp %l' "$source_path" >/dev/null 2>&1; then
  source_stat=$(stat -f '%Lp %l' "$source_path")
else
  source_stat=$(stat -c '%a %h' "$source_path")
fi
if [ "$source_stat" != "600 1" ]; then
  echo "publisher: source must have mode 0600 and one link" >&2
  exit 1
fi

source_bytes=$(wc -c < "$source_path" | tr -d ' ')
if [ "$source_bytes" -gt 4194304 ]; then
  echo "publisher: source exceeds 4 MiB" >&2
  exit 1
fi

cleanup_source() {
  rm -f -- "$source_path"
}
trap cleanup_source EXIT HUP INT TERM

if [ -e "$target_path" ] || [ -L "$target_path" ]; then
  echo "publisher: target already exists" >&2
  exit 1
fi
if ! ln -- "$source_path" "$target_path"; then
  echo "publisher: no-replace hard-link publication failed" >&2
  exit 1
fi
rm -- "$source_path"
trap - EXIT HUP INT TERM
printf '%s\n' "$target_path"
