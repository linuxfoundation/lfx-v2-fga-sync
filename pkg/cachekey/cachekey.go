// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package cachekey derives the JetStream KV key names used by fga-sync's
// access-check cache, so that both the service and out-of-process tools
// (e.g. bootstrap scripts that write tuples directly to OpenFGA) compute the
// same key for a given (object, relation) pair.
package cachekey

import "encoding/base32"

var encoder = base32.StdEncoding.WithPadding(base32.NoPadding)

// Invalidation derives the NATS-KV-safe invalidation marker key for an
// (object, relation) pair. Any cache entry for that pair created before this
// marker's timestamp is treated as stale. Base32 without padding matches the
// encoding used for "rel." cache entry keys.
func Invalidation(object, relation string) string {
	return "inv." + encoder.EncodeToString([]byte(object+"#"+relation))
}
