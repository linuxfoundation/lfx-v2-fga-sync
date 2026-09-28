// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package main provides the fga-sync service entry point and supporting types.
package main

import (
	"context"
	"expvar"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	openfga "github.com/openfga/go-sdk"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/singleflight"

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
	cacheHits      *expvar.Int
	cacheStaleHits *expvar.Int
	cacheMisses    *expvar.Int

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

// wildcardObject is the sentinel object used in an invalidationPair to mean
// "every object of this type", paired with wildcardRelation below to form a
// blanket per-type invalidation marker. No real object ID is ever literally
// "*", so it cannot collide with a genuine object+relation invalidation
// marker.
const wildcardObject = "*"

// wildcardRelation is the sentinel relation used alongside wildcardObject to
// mean "every relation of this type". No real relation name is ever
// literally "*", so it cannot collide with a genuine marker.
const wildcardRelation = "*"

// crossTypeDependents lists, for each OpenFGA object type, the OTHER types
// whose relation definitions read this type via "<relation> from <field>" in
// charts/lfx-platform/files/model.fga (a DIRECT edge only; the transitive
// closure across these edges is computed once at init by
// typeInvalidationFanout below). For example committee.writer is defined as
// "writer_guard from project", so committee depends on project, hence
// "project": {..., "committee", ...}.
//
// Same-type composition (project.writer building on project.owner,
// b2b_org.writer cascading through parent/child) needs no entry here: the
// blanket per-type marker written for the type itself (see
// expandTypeWidePairs) already invalidates every relation of that same type,
// so intra-type chains are covered without enumerating them.
//
// Keep this in sync with model.fga (owned by lfx-v2-helm): add an edge here
// whenever a relation definition gains a "from <other type>" reference,
// remove one when a relation stops referencing that type. Getting an edge
// wrong in this security-sensitive cache is asymmetric: an over-inclusive
// edge only costs extra cache misses, an under-inclusive one reintroduces
// the fail-open staleness this map exists to close.
// Type name constants for the entries below that recur across multiple map
// values (a type can be a dependent of more than one other type).
const (
	fgaTypeMeeting           = "meeting"
	fgaTypePastMeeting       = "past_meeting"
	fgaTypeProjectMembership = "project_membership"
	fgaTypeSurvey            = "survey"
	fgaTypeV1Meeting         = "v1_meeting"
	fgaTypeVote              = "vote"
)

var crossTypeDependents = map[string][]string{
	"project": {
		"mentorship_program", "committee", "groupsio_service", fgaTypeMeeting,
		fgaTypePastMeeting, fgaTypeV1Meeting, "v1_past_meeting", fgaTypeVote, fgaTypeSurvey,
		fgaTypeProjectMembership, "crowdfunding_initiative",
	},
	"mentorship_program":     {"mentorship_application"},
	"mentorship_application": {"mentorship_task"},
	"committee": {
		"committee_invite", "groupsio_mailing_list", fgaTypeMeeting, fgaTypeV1Meeting,
		fgaTypeVote, fgaTypeSurvey,
	},
	"groupsio_service":       {"groupsio_mailing_list"},
	fgaTypeMeeting:           {"meeting_attachment", fgaTypePastMeeting},
	fgaTypePastMeeting:       {"past_meeting_attachment"},
	fgaTypeV1Meeting:         {"v1_past_meeting"},
	fgaTypeVote:              {"vote_response"},
	fgaTypeSurvey:            {"survey_response"},
	"b2b_org":                {fgaTypeProjectMembership, "crowdfunding_initiative"},
	fgaTypeProjectMembership: {"b2b_org"},
}

// typeInvalidationFanout is the transitive closure of crossTypeDependents,
// computed once at init: typeInvalidationFanout[T] is every type (including
// T itself) whose blanket marker must be written when a tuple on a T object
// is written or deleted. Computed via fixed-point iteration rather than a
// naive recursive walk because the graph has at least one real cycle
// (project_membership depends on b2b_org and vice versa: model.fga defines
// b2b_org.auditor as including "key_contact from membership" while also
// defining project_membership.auditor as including "auditor from b2b_org");
// a memoized depth-first walk would cache an incomplete set for whichever
// member of the cycle it visits first.
var typeInvalidationFanout = computeTypeInvalidationFanout(crossTypeDependents)

func computeTypeInvalidationFanout(direct map[string][]string) map[string]map[string]bool {
	fanout := make(map[string]map[string]bool, len(direct))
	for t, deps := range direct {
		set := make(map[string]bool, len(deps)+1)
		set[t] = true
		for _, d := range deps {
			set[d] = true
		}
		fanout[t] = set
	}

	for changed := true; changed; {
		changed = false
		for _, set := range fanout {
			members := make([]string, 0, len(set))
			for m := range set {
				members = append(members, m)
			}
			for _, m := range members {
				for transitive := range fanout[m] {
					if !set[transitive] {
						set[transitive] = true
						changed = true
					}
				}
			}
		}
	}
	return fanout
}

// objectType returns the OpenFGA type portion of an "type:id" object
// string, or "" if object has no ':' separator.
func objectType(object string) string {
	if idx := strings.IndexByte(object, ':'); idx >= 0 {
		return object[:idx]
	}
	return ""
}

// expandTypeWidePairs returns the blanket invalidation pair(s) that must
// additionally be written for a tuple write/delete on object, on top of the
// object-scoped pair every write already gets. It always returns at least
// one pair (a blanket marker for object's own type), which closes intra-type
// transitive chains, e.g. project.viewer building on project.auditor_guard
// building on project.auditor building on project.writer building on
// project.owner, without needing to enumerate which specific relation
// depends on which. When the written type has entries in
// typeInvalidationFanout, it additionally returns a blanket marker for every
// dependent type, closing cross-type chains such as committee.writer
// depending on project.writer_guard. See crossTypeDependents for the source
// mapping this is derived from.
func expandTypeWidePairs(object string) []invalidationPair {
	typ := objectType(object)
	if typ == "" {
		return nil
	}

	types, ok := typeInvalidationFanout[typ]
	if !ok {
		return []invalidationPair{{object: typ + ":" + wildcardObject, relation: wildcardRelation}}
	}

	pairs := make([]invalidationPair, 0, len(types))
	for t := range types {
		pairs = append(pairs, invalidationPair{object: t + ":" + wildcardObject, relation: wildcardRelation})
	}
	return pairs
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
	group singleflight.Group
}

func newInvalidationLookup(cache CacheLayer) *invalidationLookup {
	return &invalidationLookup{cache: cache, memo: make(map[invalidationPair]time.Time)}
}

// get returns the last invalidation time for (object, relation): the later
// of the object-scoped marker and the blanket per-type marker for object's
// type (see expandTypeWidePairs). The blanket marker is always consulted,
// not gated to a hardcoded set of relations, because it fires on any write
// to any relation of this type, or to a relation of a type this one
// transitively depends on (typeInvalidationFanout). That closes both the
// intra-type and cross-type transitive-dependency gaps a narrower
// per-relation scheme would miss.
//
// A KV error or an unfulfilled cacheOpSem slot is treated as "just
// invalidated" (the current time) rather than propagated: the caller cannot
// verify the cached entry's freshness, so the safe default is to fall
// through to OpenFGA rather than trust a cache we could not confirm is
// fresh.
func (l *invalidationLookup) get(ctx context.Context, object, relation string) time.Time {
	t := l.getOne(ctx, object, relation)

	if typ := objectType(object); typ != "" {
		if wt := l.getOne(ctx, typ+":"+wildcardObject, wildcardRelation); wt.After(t) {
			t = wt
		}
	}

	return t
}

// getOne returns the last invalidation time for a single, literal (object,
// relation) marker, memoizing the result within this batch. Concurrent
// callers for the same pair are collapsed onto one KV Get via l.group:
// without this, every goroutine in a batch racing past the memo check
// before the first writer populates it would issue its own duplicate
// round-trip for what is, within one batch, the same read.
func (l *invalidationLookup) getOne(ctx context.Context, object, relation string) time.Time {
	pair := invalidationPair{object: object, relation: relation}

	l.mu.Lock()
	if t, ok := l.memo[pair]; ok {
		l.mu.Unlock()
		return t
	}
	l.mu.Unlock()

	key := object + "#" + relation
	v, err, _ := l.group.Do(key, func() (any, error) {
		var t time.Time
		var lookupErr error
		if slotErr := withCacheOpSlot(ctx, func() {
			t, lookupErr = l.cache.getLastInvalidation(ctx, object, relation)
		}); slotErr != nil {
			t = time.Now()
		} else if lookupErr != nil {
			logger.With(errKey, lookupErr, "object", object, "relation", relation).
				ErrorContext(ctx, "cache invalidation lookup error; treating entry as stale")
			t = time.Now()
		}

		l.mu.Lock()
		l.memo[pair] = t
		l.mu.Unlock()
		return t, nil
	})
	if err != nil {
		// fn above never returns a non-nil error; this is unreachable, but
		// treat it the same as any other lookup failure if it ever changes.
		return time.Now()
	}
	t, ok := v.(time.Time)
	if !ok {
		return time.Now()
	}
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
	cacheKey := cachekey.Entry(relationKey)
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
			cacheKey := cachekey.Entry(relationKey)
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
