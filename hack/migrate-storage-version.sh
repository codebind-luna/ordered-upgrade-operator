#!/usr/bin/env bash
# Migrates every ApplicationUpgrade to the current storage version (v1beta1)
# and then drops older versions from the CRD's status.storedVersions.
#
# Why this is needed: changing a CRD's storage version does not rewrite the
# objects already in etcd. An object written while v1alpha1 was the storage
# version stays encoded as v1alpha1 until something writes it again - and a
# Completed upgrade is never written again. Those objects stay readable through
# the conversion webhook, but the API server refuses to remove v1alpha1 from the
# CRD while it is still listed in status.storedVersions. Rewriting every object
# re-encodes it at the storage version; only then is it safe to shrink
# storedVersions and, in a later release, stop serving v1alpha1.
#
# A no-op update is enough: the API server compares the newly encoded bytes with
# what is stored, and v1beta1 bytes never equal v1alpha1 bytes, so the write
# goes through. Run this with the conversion webhook up.
set -euo pipefail

CRD=applicationupgrades.upgrades.lunadas.dev
STORAGE=v1beta1

echo "storedVersions before: $(kubectl get crd "$CRD" -o jsonpath='{.status.storedVersions}')"

# Read and write through the storage version so the update itself needs no
# down-conversion. A conflict means another writer got there first, which also
# re-encodes the object, so the rewrite is retried rather than counted as done.
failed=0
while read -r ns name; do
  [ -n "$name" ] || continue
  ok=false
  for _ in 1 2 3; do
    if out=$(kubectl -n "$ns" get "applicationupgrades.${STORAGE}.upgrades.lunadas.dev" "$name" -o json 2>&1 |
        kubectl replace -f - 2>&1); then
      ok=true; break
    fi
    case "$out" in *NotFound*|*"not found"*) ok=true; break ;; esac # deleted: nothing left to migrate
  done
  if $ok; then echo "migrated $ns/$name"; else echo "FAILED  $ns/$name: $out" >&2; failed=$((failed + 1)); fi
done < <(kubectl get "applicationupgrades.${STORAGE}.upgrades.lunadas.dev" --all-namespaces \
  -o jsonpath='{range .items[*]}{.metadata.namespace}{" "}{.metadata.name}{"\n"}{end}')

# Shrinking storedVersions while any object might still be stored at v1alpha1
# would let a later release drop v1alpha1 and strand that object unreadable.
if [ "$failed" -gt 0 ]; then
  echo "$failed object(s) not migrated; leaving storedVersions unchanged. Fix and re-run." >&2
  exit 1
fi

kubectl patch crd "$CRD" --subresource=status --type=merge \
  -p "{\"status\":{\"storedVersions\":[\"${STORAGE}\"]}}"
echo "storedVersions after: $(kubectl get crd "$CRD" -o jsonpath='{.status.storedVersions}')"
