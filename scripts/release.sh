#!/bin/sh
set -eu

# Interactive release script — suggests version bumps based on the latest tag.

# Check clean working directory
if [ -n "$(git status --porcelain)" ]; then
    echo "Error: working directory is not clean" >&2
    exit 1
fi

# Get latest tag
LATEST=$(git describe --tags --abbrev=0 2>/dev/null || echo "v0.0.0")
LATEST_NUM="${LATEST#v}"

# Parse major.minor.patch
MAJOR=$(echo "$LATEST_NUM" | cut -d. -f1)
MINOR=$(echo "$LATEST_NUM" | cut -d. -f2)
PATCH=$(echo "$LATEST_NUM" | cut -d. -f3)

NEXT_PATCH="$MAJOR.$MINOR.$((PATCH + 1))"
NEXT_MINOR="$MAJOR.$((MINOR + 1)).0"
NEXT_MAJOR="$((MAJOR + 1)).0.0"

# Show recent commits since last tag
echo ""
echo "Commits since $LATEST:"
echo "──────────────────────────────────"
git log --oneline "${LATEST}..HEAD" 2>/dev/null || git log --oneline -10
echo "──────────────────────────────────"
echo ""

# Present choices
echo "Current version: $LATEST"
echo ""
echo "  1) patch  → v$NEXT_PATCH"
echo "  2) minor  → v$NEXT_MINOR"
echo "  3) major  → v$NEXT_MAJOR"
echo "  4) custom"
echo "  0) cancel"
echo ""
printf "Select [1-4, 0 to cancel]: "
read -r CHOICE

case "$CHOICE" in
    1) NEXT="$NEXT_PATCH" ;;
    2) NEXT="$NEXT_MINOR" ;;
    3) NEXT="$NEXT_MAJOR" ;;
    4)
        printf "Enter version (without v prefix): "
        read -r NEXT
        ;;
    0|"")
        echo "Cancelled."
        exit 0
        ;;
    *)
        echo "Invalid choice." >&2
        exit 1
        ;;
esac

TAG="v${NEXT}"
echo ""
printf "Release %s? [y/N] " "$TAG"
read -r CONFIRM
case "$CONFIRM" in
    y|Y|yes|YES) ;;
    *)
        echo "Cancelled."
        exit 0
        ;;
esac

echo ""
git tag -a "$TAG" -m "Release $TAG"
git push origin "$TAG"
echo ""
echo "Tagged and pushed $TAG. GitHub Actions will build and publish the release."
