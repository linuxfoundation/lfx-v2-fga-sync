// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package main provides the fga-sync service entry point and supporting types.
package main

import (
	"context"
	"errors"
	"expvar"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/sync/errgroup"

	. "github.com/openfga/go-sdk/client"

	"github.com/linuxfoundation/lfx-v2-fga-sync/pkg/constants"
)

const (
	// globalInvalidationKey is the KV key whose creation time makes every
	// older cache entry stale.
	globalInvalidationKey = "inv"

	// typeMarkerPrefix prefixes per-object-type markers: every cached check on
	// objects of that type older than the marker is stale, for all users.
	typeMarkerPrefix = "inv.t."

	// userMarkerPrefix prefixes per-(object type, user) markers: cached checks
	// on objects of that type for that one user older than the marker are
	// stale. The user is base32-encoded like cache keys.
	userMarkerPrefix = "inv.u."

	// maxScopedUserMarkers bounds how many per-(type, user) markers one write
	// may produce. A bulk write touching many users would otherwise issue one
	// KV Put per user per affected type; past this bound the user markers fold
	// into per-type markers, which are still narrower than a global bump.
	maxScopedUserMarkers = 32

	// invalidationWriteConcurrency bounds parallel marker Puts for one write.
	invalidationWriteConcurrency = 8
	// invalidationFallbackTimeout bounds the global write that runs after
	// scoped marker writes fail.
	invalidationFallbackTimeout = 5 * time.Second

	// invalidationModeEnv selects how cache invalidation is scoped.
	invalidationModeEnv = "INVALIDATION_MODE"

	// Accepted INVALIDATION_MODE values.
	modeNameGlobal     = "global"
	modeNameReadScoped = "read-scoped"
	modeNameScoped     = "scoped"
)

var (
	cacheInvalidationsGlobal *expvar.Int
	cacheInvalidationsScoped *expvar.Int
)

func init() {
	cacheInvalidationsGlobal = expvar.NewInt("cache_invalidations_global")
	cacheInvalidationsScoped = expvar.NewInt("cache_invalidations_scoped")
}

// invalidationMode selects how writes invalidate the cache and how lookups
// honor invalidation markers. The modes exist so a rolling deploy cannot leave
// a pod writing only scoped markers while another pod, still on old code,
// ignores them and serves revoked access from cache: roll out read-scoped
// everywhere first, then switch to scoped.
type invalidationMode int

const (
	// invalidationGlobal is the original behavior: every write bumps the
	// single inv key, and lookups consult only that key.
	invalidationGlobal invalidationMode = iota
	// invalidationReadScoped makes lookups honor scoped markers while writes
	// still bump the global key. Safe to run alongside any other mode.
	invalidationReadScoped
	// invalidationScoped makes writes emit scoped markers (when the model
	// graph is available) and lookups honor them. Only safe once every
	// replica runs at least invalidationReadScoped.
	invalidationScoped
)

// parseInvalidationMode maps the INVALIDATION_MODE value to a mode. The empty
// string means the default (global). ok is false for unrecognized values,
// which callers should treat as global.
func parseInvalidationMode(value string) (mode invalidationMode, ok bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", modeNameGlobal:
		return invalidationGlobal, true
	case modeNameReadScoped:
		return invalidationReadScoped, true
	case modeNameScoped:
		return invalidationScoped, true
	}
	return invalidationGlobal, false
}

func (m invalidationMode) String() string {
	switch m {
	case invalidationReadScoped:
		return modeNameReadScoped
	case invalidationScoped:
		return modeNameScoped
	default:
		return modeNameGlobal
	}
}

// invalidationConfig is the scoping configuration carried by FgaService.
type invalidationConfig struct {
	mode  invalidationMode
	graph *modelGraph
}

// readsScoped reports whether lookups must consult scoped markers.
func (c invalidationConfig) readsScoped() bool {
	return c.mode >= invalidationReadScoped
}

// writesScoped reports whether writes may emit scoped markers. It needs the
// model graph; without one every write stays global, which is always safe.
func (c invalidationConfig) writesScoped() bool {
	return c.mode == invalidationScoped && c.graph != nil
}

// tupleRef is the part of a written or deleted tuple that decides which cache
// entries it can affect.
type tupleRef struct {
	user        string
	relation    string
	object      string
	conditioned bool
}

// userMarker names one per-(object type, user) marker.
type userMarker struct {
	typ  string
	user string
}

// invalidationPlan says which markers one write (a batch of tuples) must
// bump. A global plan bumps only the global key; otherwise types and users
// list the scoped markers.
type invalidationPlan struct {
	global bool
	types  []string
	users  []userMarker
}

// globalPlan is the plan used whenever scoping cannot be proven safe.
func globalPlan() invalidationPlan { return invalidationPlan{global: true} }

// markerKeys returns every KV key the plan writes, in a stable order.
func (p invalidationPlan) markerKeys() []string {
	keys := make([]string, 0, len(p.types)+len(p.users))
	for _, typ := range p.types {
		keys = append(keys, typeMarkerKey(typ))
	}
	for _, u := range p.users {
		keys = append(keys, userMarkerKey(u.typ, u.user))
	}
	return keys
}

func typeMarkerKey(typ string) string {
	return typeMarkerPrefix + typ
}

func userMarkerKey(typ, user string) string {
	return userMarkerPrefix + typ + "." + cacheKeyEncoder.EncodeToString([]byte(user))
}

// objectType returns the type part of an OpenFGA object ("meeting" for
// "meeting:abc"). ok is false when the object is malformed or the type holds
// characters that are not safe inside a marker key.
func objectType(object string) (string, bool) {
	typ, id, found := strings.Cut(object, ":")
	if !found || typ == "" || id == "" {
		return "", false
	}
	for _, r := range typ {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			return "", false
		}
	}
	return typ, true
}

// isConcreteUser reports whether subject names exactly one user, as opposed
// to a wildcard (user:*), a userset (team:t#member) or another object. Only a
// concrete user's tuples can be scoped to that user's checks.
func isConcreteUser(subject string) bool {
	if !strings.HasPrefix(subject, constants.ObjectTypeUser) || subject == constants.UserWildcard {
		return false
	}
	return len(subject) > len(constants.ObjectTypeUser) && !strings.Contains(subject, "#")
}

// planInvalidation decides which markers a batch of tuple changes must bump.
//
// For each tuple the model graph gives the object types whose check results
// can change. If the tuple's subject is one concrete user, only that user's
// checks can change, so the plan is a (type, user) marker per affected type.
// Any other subject (a wildcard, a userset such as team#member, or an object
// such as a parent project) can change results for an unknown set of users,
// so the plan is a type marker per affected type. A tuple on a tupleset
// relation (the "parent" in "viewer from parent") is treated the same way
// even when its subject is a user, because that user is the object being
// traversed rather than the one gaining access.
//
// Anything the graph cannot vouch for yields the global plan: no graph, a
// malformed object, a relation the model does not define, or a conditioned
// tuple whose effect depends on request context the cache key does not carry.
func (g *modelGraph) planInvalidation(tuples []tupleRef) invalidationPlan {
	if g == nil || len(tuples) == 0 {
		return globalPlan()
	}

	types := make(map[string]struct{})
	users := make(map[userMarker]struct{})

	for _, tuple := range tuples {
		if tuple.conditioned {
			return globalPlan()
		}
		typ, ok := objectType(tuple.object)
		if !ok {
			return globalPlan()
		}
		affected, ok := g.affectedTypes(typ, tuple.relation)
		if !ok {
			return globalPlan()
		}
		// A concrete user is only scoped when the tuple grants that user
		// access. On a tupleset relation the subject is the object being
		// traversed, so other users' checks through it change too.
		perUser := isConcreteUser(tuple.user) && !g.isTupleset(typ, tuple.relation)
		for _, affectedType := range affected {
			if perUser {
				users[userMarker{typ: affectedType, user: tuple.user}] = struct{}{}
			} else {
				types[affectedType] = struct{}{}
			}
		}
	}

	// Fold user markers into type markers when a bulk write would otherwise
	// issue too many Puts.
	if len(users) > maxScopedUserMarkers {
		for marker := range users {
			types[marker.typ] = struct{}{}
		}
		users = nil
	}

	plan := invalidationPlan{}
	for typ := range types {
		plan.types = append(plan.types, typ)
	}
	sort.Strings(plan.types)
	for marker := range users {
		// A type marker already covers every user of that type.
		if _, covered := types[marker.typ]; covered {
			continue
		}
		plan.users = append(plan.users, marker)
	}
	sort.Slice(plan.users, func(i, j int) bool {
		if plan.users[i].typ != plan.users[j].typ {
			return plan.users[i].typ < plan.users[j].typ
		}
		return plan.users[i].user < plan.users[j].user
	})
	return plan
}

// tupleRefs flattens the tuples of one write request for planInvalidation.
func tupleRefs(writes []ClientTupleKey, deletes []ClientTupleKeyWithoutCondition) []tupleRef {
	refs := make([]tupleRef, 0, len(writes)+len(deletes))
	for _, w := range writes {
		refs = append(refs, tupleRef{
			user:        w.User,
			relation:    w.Relation,
			object:      w.Object,
			conditioned: w.Condition != nil,
		})
	}
	for _, d := range deletes {
		refs = append(refs, tupleRef{user: d.User, relation: d.Relation, object: d.Object})
	}
	return refs
}

// applyInvalidation bumps the markers in plan. A global plan bumps only the
// global key. If any scoped marker fails to write, it falls back to the
// global key: a partially applied scoped invalidation must never leave stale
// entries alive.
func (c CacheLayer) applyInvalidation(ctx context.Context, plan invalidationPlan) error {
	if c.bucket == nil {
		return nil
	}
	if plan.global || (len(plan.types) == 0 && len(plan.users) == 0) {
		cacheInvalidationsGlobal.Add(1)
		return c.invalidate(ctx)
	}

	keys := plan.markerKeys()
	ctx, span := tracer.Start(ctx, "fga_sync.cache.invalidate",
		trace.WithAttributes(
			attribute.String("fga_sync.cache.invalidate.scope", "scoped"),
			attribute.Int("fga_sync.cache.invalidate.markers", len(keys)),
		),
	)
	defer span.End()

	var wg errgroup.Group
	wg.SetLimit(invalidationWriteConcurrency)
	for _, key := range keys {
		wg.Go(func() error {
			_, err := c.bucket.Put(ctx, key, []byte("1"))
			return err
		})
	}
	if err := wg.Wait(); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		logger.With(errKey, err).ErrorContext(ctx, "failed to write scoped invalidation markers; falling back to global")
		cacheInvalidationsGlobal.Add(1)
		// The scoped writes may have used up the caller's deadline, so the
		// fallback gets its own, or it could not write the marker even if
		// the bucket has recovered.
		fallbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), invalidationFallbackTimeout)
		defer cancel()
		return c.invalidate(fallbackCtx)
	}

	cacheInvalidationsScoped.Add(1)
	logger.With(
		"types", plan.types,
		"user_markers", len(plan.users),
	).DebugContext(ctx, "wrote scoped cache invalidation markers")
	return nil
}

// markerRead is one marker lookup result. A marker that does not exist has a
// zero time; one that could not be read is failed and makes every dependent
// entry stale, because falling through to OpenFGA is always safe.
type markerRead struct {
	at     time.Time
	failed bool
}

// invalidationCutoffs holds the invalidation times relevant to one batch of
// checks. An entry is fresh only if it was created after the cutoff for its
// own tuple.
type invalidationCutoffs struct {
	global time.Time
	types  map[string]markerRead
	users  map[userMarker]markerRead
}

// forTuple returns the latest invalidation that applies to one check: the
// global marker, the marker for the checked object's type, and the marker for
// that type and the checked user.
func (c invalidationCutoffs) forTuple(item ClientBatchCheckItem) time.Time {
	cutoff := c.global
	typ, ok := objectType(item.Object)
	if !ok {
		return cutoff
	}
	if read, found := c.types[typ]; found {
		if read.failed {
			return staleEverythingCutoff()
		}
		if read.at.After(cutoff) {
			cutoff = read.at
		}
	}
	if read, found := c.users[userMarker{typ: typ, user: item.User}]; found {
		if read.failed {
			return staleEverythingCutoff()
		}
		if read.at.After(cutoff) {
			cutoff = read.at
		}
	}
	return cutoff
}

// staleEverythingCutoff is later than any entry's creation time, so every
// entry compared against it counts as stale.
func staleEverythingCutoff() time.Time {
	return time.Now().Add(time.Hour)
}

// loadInvalidationCutoffs reads the scoped markers a batch of checks needs:
// one per distinct object type and one per distinct (type, concrete user).
// It never fails; unreadable markers are recorded as failed so the affected
// checks fall through to OpenFGA.
func (c CacheLayer) loadInvalidationCutoffs(
	ctx context.Context,
	global time.Time,
	items []ClientBatchCheckItem,
) invalidationCutoffs {
	cutoffs := invalidationCutoffs{
		global: global,
		types:  make(map[string]markerRead),
		users:  make(map[userMarker]markerRead),
	}

	typeSet := make(map[string]struct{})
	userSet := make(map[userMarker]struct{})
	for _, item := range items {
		typ, ok := objectType(item.Object)
		if !ok {
			continue
		}
		typeSet[typ] = struct{}{}
		if isConcreteUser(item.User) {
			userSet[userMarker{typ: typ, user: item.User}] = struct{}{}
		}
	}

	var mu sync.Mutex
	var wg errgroup.Group
	wg.SetLimit(cacheLookupConcurrency)

	fetch := func(key string, store func(markerRead)) {
		wg.Go(func() error {
			read := c.readMarker(ctx, key)
			mu.Lock()
			store(read)
			mu.Unlock()
			return nil
		})
	}
	for typ := range typeSet {
		fetch(typeMarkerKey(typ), func(r markerRead) { cutoffs.types[typ] = r })
	}
	for marker := range userSet {
		fetch(userMarkerKey(marker.typ, marker.user), func(r markerRead) { cutoffs.users[marker] = r })
	}
	//nolint:errcheck // every goroutine above returns nil; Wait only blocks.
	_ = wg.Wait()
	return cutoffs
}

// readMarker reads one marker key.
func (c CacheLayer) readMarker(ctx context.Context, key string) markerRead {
	var (
		created time.Time
		err     error
	)
	if slotErr := withCacheOpSlot(ctx, func() {
		created, err = c.markerCreated(ctx, key)
	}); slotErr != nil {
		return markerRead{failed: true}
	}
	if err != nil {
		logger.With(errKey, err).WarnContext(ctx, "failed to read invalidation marker; treating entries as stale")
		return markerRead{failed: true}
	}
	return markerRead{at: created}
}

// markerCreated returns the creation time of a marker key, or the zero time
// when it does not exist.
func (c CacheLayer) markerCreated(ctx context.Context, key string) (time.Time, error) {
	entry, err := c.bucket.Get(ctx, key)
	switch {
	case errors.Is(err, jetstream.ErrKeyNotFound):
		return time.Time{}, nil
	case err != nil:
		return time.Time{}, err
	}
	return entry.Created(), nil
}
