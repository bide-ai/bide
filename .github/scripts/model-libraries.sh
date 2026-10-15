#!/usr/bin/env bash
# model-libraries.sh: the TLA+ models whose configurations name one of the given models in a LIBRARY
# line (model 14's composition with model 10), so a change to the named model must re-check them.
#
#   model-libraries.sh MODEL...      the models, one per line, from spec/tla/*/*.cfg and
#                                    spec/tla/*/*/*.cfg, run from the repository root
#   model-libraries.sh --self-test   prove the match takes the whitespace check.sh accepts
#
# spec/tla/check.sh reads the line with sed "s/^\* LIBRARY: *//" and splits the value on any
# whitespace, so the names may be separated by spaces or tabs and the line may end with either.
set -euo pipefail

libraries() { # libraries ROOT MODEL...: the model directories under ROOT/spec/tla naming one
  local root=$1 d m
  shift
  for d in "$root"/spec/tla/*/; do
    d=$(basename "$d")
    for m in "$@"; do
      if grep -qsE "^.. LIBRARY:[[:space:]]*([^[:space:]]+[[:space:]]+)*$m([[:space:]]|\$)" "$root"/spec/tla/"$d"/*.cfg "$root"/spec/tla/"$d"/*/*.cfg; then
        echo "$d"
      fi
    done
  done
}

if [ "${1:-}" = --self-test ]; then
  tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
  for d in a b c d e f h i; do mkdir -p "$tmp/spec/tla/$d"; done
  mkdir -p "$tmp/spec/tla/g/limits"
  printf '\\* LIBRARY: m\n' > "$tmp/spec/tla/a/x.cfg"
  printf '\\* LIBRARY: other  m\n' > "$tmp/spec/tla/b/x.cfg"
  printf '\\* LIBRARY: other\tm\n' > "$tmp/spec/tla/c/x.cfg"
  printf '\\* LIBRARY:  m  other\n' > "$tmp/spec/tla/d/x.cfg"
  printf '\\* LIBRARY: mm\n' > "$tmp/spec/tla/e/x.cfg"
  printf '\\* LIBRARY: other\n' > "$tmp/spec/tla/f/x.cfg"
  printf '\\* LIBRARY:m\n' > "$tmp/spec/tla/h/x.cfg"
  printf '\\* LIBRARY:other m\n' > "$tmp/spec/tla/i/x.cfg"
  printf '\\* LIBRARY: other\tm\n' > "$tmp/spec/tla/g/limits/x.cfg"
  got=$(libraries "$tmp" m | LC_ALL=C sort -u | tr '\n' ' ')
  [ "$got" = "a b c d g h i " ] || { echo "self-test: got '$got', want 'a b c d g h i '"; exit 1; }
  echo "self-test: ok"; exit 0
fi
[ $# -ge 1 ] || { echo "usage: $0 MODEL... | --self-test" >&2; exit 2; }
libraries . "$@"
