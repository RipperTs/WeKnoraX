#!/usr/bin/env bash
set -euo pipefail

if ! command -v git >/dev/null 2>&1; then
  echo "Required command not found: git" >&2
  exit 1
fi

resolve_directory() {
  cd -- "$1"
  pwd -P
}

resolve_git_directory() {
  local path="$1"

  if [[ "$path" != /* ]]; then
    path="${worktree_root}/${path}"
  fi
  resolve_directory "$path"
}

is_managed_local_file() {
  case "$1" in
    .env | \
      .env.local | \
      .env.lite | \
      frontend/.env | \
      frontend/.env.local | \
      frontend/.env.development | \
      frontend/.env.development.local | \
      frontend/.env.production | \
      frontend/.env.production.local)
      return 0
      ;;
    *)
      return 1
      ;;
  esac
}

remove_worktree_path() {
  local relative_path="$1"
  local target_path

  if [[ -z "$relative_path" || "$relative_path" == /* || "$relative_path" == *".."* ]]; then
    echo "Refusing to remove unsafe path: ${relative_path}" >&2
    return 1
  fi

  target_path="${worktree_root}/${relative_path}"
  if [[ -e "$target_path" || -L "$target_path" ]]; then
    rm -rf -- "$target_path"
    echo "Removed: ${relative_path}"
  fi
}

worktree_root="$(resolve_directory "$(git rev-parse --show-toplevel)")"
git_common_dir="$(resolve_git_directory "$(git rev-parse --git-common-dir)")"
git_dir="$(resolve_git_directory "$(git rev-parse --git-dir)")"
primary_root="$(resolve_directory "${git_common_dir}/..")"
state_file="${git_dir}/codex-worktree-managed-files"

if [[ "$git_dir" == "$git_common_dir" || "$worktree_root" == "$primary_root" ]]; then
  echo "This script only cleans linked Git worktrees." >&2
  exit 1
fi

remove_worktree_path "frontend/node_modules"

if [[ -f "$state_file" ]]; then
  while IFS= read -r relative_path; do
    if is_managed_local_file "$relative_path"; then
      remove_worktree_path "$relative_path"
    else
      echo "Skipped unrecognized managed path: ${relative_path}" >&2
    fi
  done <"$state_file"
  rm -f -- "$state_file"
fi

echo "Codex worktree cleanup completed."
