#!/usr/bin/env bash
set -euo pipefail

bad=0
while IFS= read -r -d '' path; do
  case "$path" in
    prompts/*|operator-notes/*|docs/superpowers/*|docs/*prompt*.md|docs/*-plan.md|docs/*-design.md|docs/*-acceptance.md)
      printf 'Internal-only file is tracked: %s\n' "$path" >&2
      bad=1
      ;;
  esac
done < <(git ls-files -z)

if (( bad )); then
  exit 1
fi

printf 'Public tree path check passed.\n'
