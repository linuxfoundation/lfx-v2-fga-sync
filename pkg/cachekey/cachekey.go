// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package cachekey derives the JetStream KV key names used by fga-sync's
// access-check cache, so that both the service and out-of-process tools
// (e.g. bootstrap scripts that write tuples directly to OpenFGA) compute the
// same key for a given (object, relation) pair.
package cachekey

import "encoding/base32"

var encoder = base32.StdEncoding.WithPadding(base32.NoPadding)

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
