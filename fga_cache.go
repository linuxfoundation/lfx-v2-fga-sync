// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package main provides the fga-sync service entry point and supporting types.
package main

import (
	"context"
	"encoding/base32"
	"expvar"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	openfga "github.com/openfga/go-sdk"
	"golang.org/x/sync/errgroup"

	. "github.com/openfga/go-sdk/client"
)

const (
	// cacheLookupConcurrency bounds how many NATS KV Get/Put calls run in
	// parallel when resolving a batch of tuples against the cache. A batch of
	// several hundred tuples run serially (one JetStream round-trip each, ~20ms
	// apiece) was directly responsible for double-digit-second access-check
	// latency; this trades a bounded amount of extra NATS load for wall-clock.
	cacheLookupConcurrency = 64

	// cacheOpConcurrency caps total in-flight JetStream KV operations for this
	// process. cacheLookupConcurrency bounds one request's fan-out, but the
	// subscription layer admits subscriptionConcurrency (64) handlers at once,
	// so per-request limits alone permit ~4,096 simultaneous KV round-trips per
	// pod. The cache bucket is single-replica (see the chart's
	// nats-kv-bucket.yaml, which sets no replicas field), so every pod's cache
	// traffic funnels into one JetStream node; cluster-wide pressure is this
	// value times application.replicas (3 in prod). Sized at 2x
	// cacheLookupConcurrency so a couple of large batches still overlap fully
	// while that product stays reasonable for a single-node bucket.
	cacheOpConcurrency = 2 * cacheLookupConcurrency
)

var (
	cacheHits       *expvar.Int
	cacheStaleHits  *expvar.Int
	cacheMisses     *expvar.Int
	cacheKeyEncoder = base32.StdEncoding.WithPadding(base32.NoPadding)

	// cacheOpSem is the service-wide budget for JetStream KV operations. Every
	// concurrent KV Get/Put in this file acquires a slot before the
	// round-trip and releases it after. Held only around the KV call itself,
	// never across an OpenFGA call, so a slow BatchCheck cannot hold cache
	// capacity hostage.
	cacheOpSem = make(chan struct{}, cacheOpConcurrency)
)

func init() {
	cacheHits = expvar.NewInt("cache_hits")
	cacheStaleHits = expvar.NewInt("cache_stale_hits")
	cacheMisses = expvar.NewInt("cache_misses")
}

// withCacheOpSlot runs fn while holding a slot in the service-wide KV budget
// (cacheOpSem). If ctx is canceled before a slot frees, fn is not run and
// ctx.Err() is returned; callers treat that the same as any other cache
// failure (fall through to OpenFGA, or skip a best-effort write).
func withCacheOpSlot(ctx context.Context, fn func()) error {
	select {
	case cacheOpSem <- struct{}{}:
		defer func() { <-cacheOpSem }()
		fn()
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// cacheLookupOutcome is the result of resolving a single tuple against the
// cache: either a ready-to-append response line, or a signal that the tuple
// still needs to be resolved via OpenFGA.
type cacheLookupOutcome struct {
	needsCheck bool
	hitLine    []byte
}

// CacheLayer wraps the JetStream KV bucket and owns all cache operations:
// invalidation key management, staleness checks, per-tuple lookup, positive-
// entry seeding, and write-back after a BatchCheck. It has no knowledge of
// OpenFGA tuple semantics or the FGA client; callers supply pre-computed
// relation-key strings and BatchCheck results.
type CacheLayer struct {
	bucket INatsKeyValue
}

// invalidate writes the inv timestamp key. Any cached entry whose KV creation
// time predates this write is treated as stale on the next lookup. It is a
// no-op when the bucket is nil (e.g., cache disabled or not yet connected).
func (c CacheLayer) invalidate(ctx context.Context) error {
	if c.bucket == nil {
		return nil
	}
	_, err := c.bucket.Put(ctx, "inv", []byte("1"))
	if err != nil {
		logger.With(errKey, err).ErrorContext(ctx, "failed to write cache invalidation marker")
		return err
	}
	return nil
}

// getLastInvalidation reads the inv key and returns its creation time, or the
// zero time when no invalidation has been recorded within the TTL window.
func (c CacheLayer) getLastInvalidation(ctx context.Context) (time.Time, error) {
	var lastInvalidation time.Time
	entry, err := c.bucket.Get(ctx, "inv")
	switch {
	case err == jetstream.ErrKeyNotFound:
		// No invalidation in the TTL of the cache; all found cache entries are
		// valid. Keep the zero-value of lastInvalidation.
	case err != nil:
		return time.Time{}, err
	default:
		lastInvalidation = entry.Created()
	}
	return lastInvalidation, nil
}

// lookupEntry checks the KV cache for a single tuple check item. It never
// returns a Go error: a miss, a stale hit, or an unexpected KV error all
// set needsCheck so the tuple is forwarded to OpenFGA instead of dropped.
func (c CacheLayer) lookupEntry(
	ctx context.Context,
	tuple ClientBatchCheckItem,
	lastInvalidation time.Time,
) cacheLookupOutcome {
	relationKey := tuple.Object + "#" + tuple.Relation + "@" + tuple.User
	// Encode relation using base32 without padding to conform to the allowed
	// characters for NATS subjects.
	cacheKey := "rel." + cacheKeyEncoder.EncodeToString([]byte(relationKey))
	var entry jetstream.KeyValueEntry
	var errCache error
	if slotErr := withCacheOpSlot(ctx, func() {
		entry, errCache = c.bucket.Get(ctx, cacheKey)
	}); slotErr != nil {
		// The service-wide KV budget didn't free up before ctx was canceled;
		// treat this the same as any other cache miss so the tuple falls
		// through to OpenFGA.
		cacheMisses.Add(1)
		return cacheLookupOutcome{needsCheck: true}
	}
	switch {
	case errCache == jetstream.ErrKeyNotFound:
		cacheMisses.Add(1)
		return cacheLookupOutcome{needsCheck: true}
	case errCache != nil:
		// This is not expected (we would have exited early already on cache
		// errors when grabbing the invalidation timestamp), but log and treat
		// this single tuple as a miss rather than failing the whole request.
		logger.With(errKey, errCache).ErrorContext(ctx, "cache error; treating as miss")
		cacheMisses.Add(1)
		return cacheLookupOutcome{needsCheck: true}
	}

	// Cache entry was found. If the cache entry is older than the invalidation
	// timestamp, skip it.
	if lastInvalidation.After(entry.Created()) {
		logger.With(
			"relation_key", relationKey,
			"last_invalidation", lastInvalidation,
			"entry_created", entry.Created(),
			"entry_value", string(entry.Value()),
		).DebugContext(ctx, "cache stale hit")
		cacheStaleHits.Add(1)
		return cacheLookupOutcome{needsCheck: true}
	}

	logger.With(
		"relation_key", relationKey,
		"last_invalidation", lastInvalidation,
		"entry_created", entry.Created(),
		"entry_value", string(entry.Value()),
	).DebugContext(ctx, "cache hit")
	cacheHits.Add(1)
	return cacheLookupOutcome{
		hitLine: []byte(fmt.Sprintf("%s\t%s\n", relationKey, string(entry.Value()))),
	}
}

// seedPositiveEntries writes a trueString cache entry for each key, blocking
// until all writes complete (or time out). Callers must only pass keys for
// tuples that were successfully written to OpenFGA.
//
// seedPositiveEntries blocks until all cache writes complete, so by the time
// it returns the writes have either already happened or will never happen; no
// additional wait is needed by callers. It is called after cache invalidation
// so a stale seed cannot resurrect access a prior message had just revoked.
//
// Each write additionally waits on cacheOpSem, the service-wide JetStream KV
// budget shared with CheckRelationships. That only ever shortens how many
// writes run at once; it does not affect the wg.Wait() ordering guarantee
// above, since every goroutine below is still awaited regardless of whether
// it ran immediately or queued for a slot.
func (c CacheLayer) seedPositiveEntries(ctx context.Context, cacheKeys []string) {
	var wg sync.WaitGroup
	for _, cacheKey := range cacheKeys {
		wg.Add(1)
		go func(key string) {
			defer wg.Done()
			timeoutCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()

			//nolint:errcheck // Cache seeding is best-effort after a successful OpenFGA write.
			_ = withCacheOpSlot(timeoutCtx, func() {
				_, _ = c.bucket.PutString(timeoutCtx, key, trueString)
			})
		}(cacheKey)
	}
	wg.Wait()
}

// buildResponseAndWriteBack assembles result lines from a BatchCheck response
// and fans out bounded-concurrency cache Puts for every non-error result. It
// appends to message (which may already contain cache-hit lines from a prior
// parallel lookup pass) and returns the extended slice.
//
// Cache write-backs are fanned out with bounded concurrency once the message
// is assembled. g.Wait() blocks before this function returns, so they
// complete within the request's lifetime; the fan-out only bounds how many
// run at once, not whether the reply waits for them.
func (c CacheLayer) buildResponseAndWriteBack(
	ctx context.Context,
	message []byte,
	result map[string]openfga.BatchCheckSingleResult,
	mapCorrelationIDToTuple map[string]ClientBatchCheckItem,
) []byte {
	cachePuts := make([]func() error, 0, len(result))

	for correlationID, resp := range result {
		// This is the specific request tuple that the response corresponds to.
		req, ok := mapCorrelationIDToTuple[correlationID]
		if !ok {
			continue
		}
		relationKey := req.Object + "#" + req.Relation + "@" + req.User

		// Need a bool to handle whether or not a response should be cached.
		// This is needed since it may be an error and not a valid response, but
		// we still need to return not allowed and not cache it.
		shouldCache := true
		// Check if the response contains an error (e.g., timeout, deadline
		// exceeded) and skip caching/responding with error results.
		if resp.HasError() {
			checkErr := resp.GetError()
			logger.With(
				"correlation_id", correlationID,
				"relation_key", relationKey,
				"error_code", checkErr.GetInternalError(),
				"error_message", checkErr.GetMessage(),
			).WarnContext(ctx, "batch check returned error for tuple, skipping cache")
			shouldCache = false
		}

		allowed := strconv.FormatBool(resp.GetAllowed())

		// Append the result to our response message.
		message = append(message, []byte(relationKey+"\t"+allowed+"\n")...)

		// Queue the cache write.
		if shouldCache {
			cacheKey := "rel." + cacheKeyEncoder.EncodeToString([]byte(relationKey))
			allowedValue := allowed
			cachePuts = append(cachePuts, func() error {
				putCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				defer cancel()
				if slotErr := withCacheOpSlot(putCtx, func() {
					if _, err := c.bucket.Put(putCtx, cacheKey, []byte(allowedValue)); err != nil {
						logger.With(errKey, err).ErrorContext(ctx, "failed to cache relation")
					}
				}); slotErr != nil {
					logger.With(errKey, slotErr).ErrorContext(ctx, "failed to cache relation")
				}
				return nil
			})
		}
	}

	if len(cachePuts) > 0 {
		g, gctx := errgroup.WithContext(ctx)
		g.SetLimit(cacheLookupConcurrency)
		for _, put := range cachePuts {
			g.Go(func() error {
				select {
				case <-gctx.Done():
					return nil
				default:
					return put()
				}
			})
		}
		// Every closure above always returns nil (cache-write failures are
		// already logged per-entry, not propagated), so g.Wait() can only
		// ever return nil here; it is called solely to block until all
		// writes finish.
		//nolint:errcheck // g.Wait() can only return nil; see comment above.
		_ = g.Wait()
	}

	return message
}
