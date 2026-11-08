#!/bin/sh
# install-git-hooks.sh — install the shared git hooks from scripts/git-hooks/.
#
# Git only reads hooks from .git/hooks, which is not versioned, so a fresh
# clone has none. Run this once after cloning (or after we add a hook):
#
#     ./scripts/install-git-hooks.sh
#
# It overwrites the hooks it manages, so keep any private hook you care
# about somewhere safe before running it.

set -u

cd "$(dirname "$0")/.." || exit 1

for hook in scripts/git-hooks/*; do
    name=$(basename "$hook")
    cp "$hook" ".git/hooks/$name"
    chmod +x ".git/hooks/$name"
    echo "installed .git/hooks/$name"
done
