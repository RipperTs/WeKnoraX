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

record_managed_file() {
  local relative_path="$1"

  if grep -Fqx -- "$relative_path" "$state_file"; then
    return
  fi
  printf '%s\n' "$relative_path" >>"$state_file"
}

copy_local_file() {
  local relative_path="$1"
  local source_path="${primary_root}/${relative_path}"
  local target_path="${worktree_root}/${relative_path}"

  if [[ -e "$target_path" || -L "$target_path" || ! -f "$source_path" ]]; then
    return
  fi

  mkdir -p -- "$(dirname -- "$target_path")"
  cp -p -- "$source_path" "$target_path"
  record_managed_file "$relative_path"
  echo "Copied local file: ${relative_path}"
}

ensure_node_24() {
  local node_major=""
  local nvm_script="${NVM_DIR:-${HOME}/.nvm}/nvm.sh"

  if command -v node >/dev/null 2>&1; then
    node_major="$(node -p "process.versions.node.split('.')[0]" 2>/dev/null || true)"
  fi
  if [[ "$node_major" == "24" ]]; then
    return
  fi
  if [[ ! -s "$nvm_script" ]]; then
    echo "Node.js 24 is required, but nvm was not found at ${nvm_script}." >&2
    exit 1
  fi

  # shellcheck source=/dev/null
  source "$nvm_script"
  nvm install 24
}

worktree_root="$(resolve_directory "$(git rev-parse --show-toplevel)")"
git_common_dir="$(resolve_git_directory "$(git rev-parse --git-common-dir)")"
git_dir="$(resolve_git_directory "$(git rev-parse --git-dir)")"
primary_root="$(resolve_directory "${git_common_dir}/..")"
state_file="${git_dir}/codex-worktree-managed-files"

if [[ "$git_dir" == "$git_common_dir" || "$worktree_root" == "$primary_root" ]]; then
  echo "This script only initializes linked Git worktrees." >&2
  exit 1
fi

if ! command -v go >/dev/null 2>&1; then
  echo "Required command not found: go" >&2
  exit 1
fi

ensure_node_24
if ! command -v npm >/dev/null 2>&1; then
  echo "Required command not found: npm" >&2
  exit 1
fi

touch "$state_file"

while IFS= read -r relative_path; do
  copy_local_file "$relative_path"
done <<'EOF'
.env
.env.local
.env.lite
frontend/.env
frontend/.env.local
frontend/.env.development
frontend/.env.development.local
frontend/.env.production
frontend/.env.production.local
EOF

echo "Downloading Go dependencies"
(
  cd -- "$worktree_root"
  go mod download
)

echo "Installing frontend dependencies"
(
  cd -- "${worktree_root}/frontend"
  npm ci --prefer-offline
)

echo "Codex worktree initialization completed."
