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
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/sync/errgroup"

	. "github.com/openfga/go-sdk/client"

	"github.com/linuxfoundation/lfx-v2-fga-sync/pkg/cachekey"
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

// cacheOutcomeKind classifies how a cacheLookupOutcome was resolved, so
// callers can tally hits/stale-hits/misses for tracing without re-deriving
// the classification from needsCheck and hitLine.
type cacheOutcomeKind int

const (
	cacheOutcomeMiss cacheOutcomeKind = iota
	cacheOutcomeHit
	cacheOutcomeStaleHit
)

// cacheLookupOutcome is the result of resolving a single tuple against the
// cache: either a ready-to-append response line, or a signal that the tuple
// still needs to be resolved via OpenFGA.
type cacheLookupOutcome struct {
	kind       cacheOutcomeKind
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

// invalidationPair identifies the (object, relation) scope of a cache
// invalidation marker. Invalidation is scoped to object+relation rather than
// per-user because that is the granularity a write batch reports (see
// WriteAndDeleteTuples): OpenFGA does not report which users hold a relation
// that was deleted by a non-tuple-enumerating condition, so anything more
// precise would require reading the tuples back before invalidating.
type invalidationPair struct {
	object   string
	relation string
}

// invalidate writes an invalidation marker for every unique (object,
// relation) pair in pairs, fanned out with the same bounded concurrency used
// for cache write-backs so a large multi-pair batch cannot serialize into N
// sequential JetStream round-trips inside the caller's invalidation timeout.
// Any cached entry for that object and relation (across all users) whose KV
// creation time predates this write is treated as stale on the next lookup.
// Per-pair failures are logged, not propagated: a failed marker write means
// that pair's cache entries may serve stale results until the next
// successful invalidation, which is preferable to failing the write/delete
// that triggered it. It is a no-op when the bucket is nil (e.g., cache
// disabled or not yet connected) or pairs is empty.
func (c CacheLayer) invalidate(ctx context.Context, pairs []invalidationPair) {
	if c.bucket == nil || len(pairs) == 0 {
		return
	}

	ctx, span := tracer.Start(ctx, "fga_sync.cache.invalidate")
	defer span.End()

	seen := make(map[invalidationPair]struct{}, len(pairs))
	unique := make([]invalidationPair, 0, len(pairs))
	for _, pair := range pairs {
		if _, ok := seen[pair]; ok {
			continue
		}
		seen[pair] = struct{}{}
		unique = append(unique, pair)
	}

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(cacheLookupConcurrency)
	for _, pair := range unique {
		g.Go(func() error {
			if slotErr := withCacheOpSlot(gctx, func() {
				if _, err := c.bucket.Put(gctx, cachekey.Invalidation(pair.object, pair.relation), []byte("1")); err != nil {
					logger.With(
						errKey, err,
						"object", pair.object,
						"relation", pair.relation,
					).ErrorContext(ctx, "failed to write cache invalidation marker")
				}
			}); slotErr != nil {
				logger.With(
					errKey, slotErr,
					"object", pair.object,
					"relation", pair.relation,
				).ErrorContext(ctx, "failed to write cache invalidation marker")
			}
			return nil
		})
	}
	// Every closure above always returns nil (failures are logged per-pair,
	// not propagated), so g.Wait() can only ever return nil here; it is
	// called solely to block until every marker write finishes.
	//nolint:errcheck // g.Wait() can only return nil; see comment above.
	_ = g.Wait()

	span.SetAttributes(
		attribute.Int("fga_sync.cache.invalidate.pairs_total", len(pairs)),
		attribute.Int("fga_sync.cache.invalidate.pairs_unique", len(unique)),
	)
}

// getLastInvalidation reads the invalidation marker for a single (object,
// relation) pair and returns its creation time, or the zero time when no
// invalidation has been recorded within the TTL window.
func (c CacheLayer) getLastInvalidation(ctx context.Context, object, relation string) (time.Time, error) {
	var lastInvalidation time.Time
	entry, err := c.bucket.Get(ctx, cachekey.Invalidation(object, relation))
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

// invalidationLookup memoizes per-(object, relation) invalidation timestamps
// within a single CheckRelationships batch. Many batches request several
// relations for the same object (or the same relation across a handful of
// objects), so memoizing keeps this at one KV Get per unique pair rather
// than one per tuple.
type invalidationLookup struct {
	cache CacheLayer
	mu    sync.Mutex
	memo  map[invalidationPair]time.Time
}

func newInvalidationLookup(cache CacheLayer) *invalidationLookup {
	return &invalidationLookup{cache: cache, memo: make(map[invalidationPair]time.Time)}
}

// get returns the last invalidation time for (object, relation). A KV error
// or an unfulfilled cacheOpSem slot is treated as "just invalidated" (the
// current time) rather than propagated: the caller cannot verify the cached
// entry's freshness, so the safe default is to fall through to OpenFGA
// rather than trust a cache we could not confirm is fresh.
func (l *invalidationLookup) get(ctx context.Context, object, relation string) time.Time {
	pair := invalidationPair{object: object, relation: relation}

	l.mu.Lock()
	if t, ok := l.memo[pair]; ok {
		l.mu.Unlock()
		return t
	}
	l.mu.Unlock()

	var t time.Time
	var err error
	if slotErr := withCacheOpSlot(ctx, func() {
		t, err = l.cache.getLastInvalidation(ctx, object, relation)
	}); slotErr != nil {
		t = time.Now()
	} else if err != nil {
		logger.With(errKey, err, "object", object, "relation", relation).
			ErrorContext(ctx, "cache invalidation lookup error; treating entry as stale")
		t = time.Now()
	}

	l.mu.Lock()
	l.memo[pair] = t
	l.mu.Unlock()
	return t
}

// lookupEntry checks the KV cache for a single tuple check item. It never
// returns a Go error: a miss, a stale hit, or an unexpected KV error all
// set needsCheck so the tuple is forwarded to OpenFGA instead of dropped.
func (c CacheLayer) lookupEntry(
	ctx context.Context,
	tuple ClientBatchCheckItem,
	invLookup *invalidationLookup,
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
		return cacheLookupOutcome{kind: cacheOutcomeMiss, needsCheck: true}
	}
	switch {
	case errCache == jetstream.ErrKeyNotFound:
		cacheMisses.Add(1)
		return cacheLookupOutcome{kind: cacheOutcomeMiss, needsCheck: true}
	case errCache != nil:
		// This is not expected, but log and treat this single tuple as a miss
		// rather than failing the whole request.
		logger.With(errKey, errCache).ErrorContext(ctx, "cache error; treating as miss")
		cacheMisses.Add(1)
		return cacheLookupOutcome{kind: cacheOutcomeMiss, needsCheck: true}
	}

	// Cache entry was found. If the cache entry is older than this tuple's
	// object+relation invalidation timestamp, skip it.
	lastInvalidation := invLookup.get(ctx, tuple.Object, tuple.Relation)
	if lastInvalidation.After(entry.Created()) {
		logger.With(
			"relation_key", relationKey,
			"last_invalidation", lastInvalidation,
			"entry_created", entry.Created(),
			"entry_value", string(entry.Value()),
		).DebugContext(ctx, "cache stale hit")
		cacheStaleHits.Add(1)
		return cacheLookupOutcome{kind: cacheOutcomeStaleHit, needsCheck: true}
	}

	logger.With(
		"relation_key", relationKey,
		"last_invalidation", lastInvalidation,
		"entry_created", entry.Created(),
		"entry_value", string(entry.Value()),
	).DebugContext(ctx, "cache hit")
	cacheHits.Add(1)
	return cacheLookupOutcome{
		kind:    cacheOutcomeHit,
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
	if len(cacheKeys) == 0 {
		return
	}
	ctx, span := tracer.Start(ctx, "fga_sync.cache.seed",
		trace.WithAttributes(attribute.Int("fga_sync.cache.seed.keys", len(cacheKeys))),
	)
	defer span.End()

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
		putCtx, putSpan := tracer.Start(ctx, "fga_sync.cache.write_back",
			trace.WithAttributes(attribute.Int("fga_sync.cache.write_back.puts", len(cachePuts))),
		)

		g, gctx := errgroup.WithContext(putCtx)
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
		putSpan.End()
	}

	return message
}
