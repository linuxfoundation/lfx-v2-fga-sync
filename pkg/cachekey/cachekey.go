// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package cachekey derives the JetStream KV key names used by fga-sync's
// access-check cache, so that both the service and out-of-process tools
// (e.g. bootstrap scripts that write tuples directly to OpenFGA) compute the
// same key for a given (object, relation) pair.
package cachekey

import "encoding/base32"

var encoder = base32.StdEncoding.WithPadding(base32.NoPadding)

// LegacyInvalidationKey is the single global invalidation marker written by
// pre-relinv-rollout code: a bare "inv" key with no prefix or encoding,
// bumped on every write/delete regardless of which object or relation it
// touched. It is exported from this package (rather than kept private to
// the fga-sync main package) so that out-of-process direct-OpenFGA writers
// — e.g. scripts/bootstrap/member-tiers-callers — can dual-write it too.
//
// ROLLOUT COMPATIBILITY, REMOVE AFTER FULL ROLLOUT: during a rolling
// deploy, a pod may be running old code that only knows this bare key (both
// as a reader and a writer), while a pod running new code writes/reads the
// "inv."-prefixed per-(object, relation) markers instead. Until every pod in
// the fleet (and every out-of-process writer) is confirmed running
// post-relinv code, every writer must dual-write this key alongside its
// scoped marker(s), and every reader must additionally consult it, or a pod
// on one side of the rollout can miss an invalidation the other side
// processed. Delete this constant and every write/read call site once that
// is confirmed.
const LegacyInvalidationKey = "inv"

// Entry derives the NATS-KV-safe cache entry key for a full relation tuple
// key (object#relation@user). Prefixed "relinv." rather than the older
// "rel.", because entries under this prefix are only ever written by code
// that also maintains the "inv." invalidation markers below. A pod running
// pre-invalidation-marker code has no notion of those markers, so if it
// reused the "rel." prefix during a rolling deploy, it could refresh an
// entry's timestamp past a marker set by an already-upgraded pod, making a
// revoked entry look fresh again. Giving invalidation-aware writes their own
// disjoint prefix means an old pod's writes can never collide with a new
// pod's read; they just age out under the old, unread "rel." prefix via the
// bucket's own TTL. Do not reuse "rel." for anything, and do not "bump" this
// prefix again for unrelated changes — it exists to solve this exact
// mixed-version hazard once, not to version the cache format going forward.
func Entry(relationKey string) string {
	return "relinv." + encoder.EncodeToString([]byte(relationKey))
}

// Invalidation derives the NATS-KV-safe invalidation marker key for an
// (object, relation) pair. Any cache entry for that pair created before this
// marker's timestamp is treated as stale. Base32 without padding matches the
// encoding used for Entry's cache entry keys.
func Invalidation(object, relation string) string {
	return "inv." + encoder.EncodeToString([]byte(object+"#"+relation))
}
