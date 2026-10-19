// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/openfga/go-sdk/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/linuxfoundation/lfx-v2-fga-sync/pkg/cachekey"
	"github.com/linuxfoundation/lfx-v2-fga-sync/pkg/constants"
)

type cacheWriteRecorder struct {
	writes chan string
}

func (r *cacheWriteRecorder) Get(context.Context, string) (jetstream.KeyValueEntry, error) {
	return nil, jetstream.ErrKeyNotFound
}

func (r *cacheWriteRecorder) Put(context.Context, string, []byte) (uint64, error) {
	return 1, nil
}

func (r *cacheWriteRecorder) PutString(_ context.Context, key, _ string) (uint64, error) {
	r.writes <- key
	return 1, nil
}

func TestCacheLayerInvalidateDedupesPairs(t *testing.T) {
	kv := new(MockNatsKeyValue)
	kv.On("Put", mock.Anything, mock.AnythingOfType("string"), []byte("1")).Return(uint64(1), nil)
	cache := CacheLayer{bucket: kv}

	pairs := []invalidationPair{
		{object: "project:1", relation: "viewer"},
		{object: "project:1", relation: "viewer"},
		{object: "project:2", relation: "viewer"},
	}
	cache.invalidate(context.Background(), pairs)

	// Three Put calls: the two deduped scoped markers, plus the legacy
	// global "inv" marker that invalidate() always dual-writes alongside
	// the scoped ones for rolling-deployment compatibility (see
	// cachekey.LegacyInvalidationKey).
	kv.AssertNumberOfCalls(t, "Put", 3)
	kv.AssertCalled(t, "Put", mock.Anything, cachekey.Invalidation("project:1", "viewer"), []byte("1"))
	kv.AssertCalled(t, "Put", mock.Anything, cachekey.Invalidation("project:2", "viewer"), []byte("1"))
	kv.AssertCalled(t, "Put", mock.Anything, cachekey.LegacyInvalidationKey, []byte("1"))
}

// TestCacheLayerInvalidateDualWritesLegacyMarker is the dedicated regression
// test for the rolling-deployment compatibility fix: every call to
// invalidate() must dual-write the legacy global "inv" key alongside its
// scoped (object, relation) markers, even when only a single pair is
// invalidated, so that an fga-sync pod still running pre-relinv code (which
// only ever reads that bare key) observes the invalidation too. See
// cachekey.LegacyInvalidationKey and the "Rolling deployment: legacy global
// inv marker" section of docs/fga-sync-contract.md.
func TestCacheLayerInvalidateDualWritesLegacyMarker(t *testing.T) {
	kv := new(MockNatsKeyValue)
	kv.On("Put", mock.Anything, mock.AnythingOfType("string"), []byte("1")).Return(uint64(1), nil)
	cache := CacheLayer{bucket: kv}

	cache.invalidate(context.Background(), []invalidationPair{{object: "project:1", relation: "viewer"}})

	kv.AssertNumberOfCalls(t, "Put", 2)
	kv.AssertCalled(t, "Put", mock.Anything, cachekey.Invalidation("project:1", "viewer"), []byte("1"))
	kv.AssertCalled(t, "Put", mock.Anything, cachekey.LegacyInvalidationKey, []byte("1"))
}

func TestCacheLayerInvalidateNoOpOnEmptyOrNilBucket(t *testing.T) {
	kv := new(MockNatsKeyValue)
	cache := CacheLayer{bucket: kv}
	cache.invalidate(context.Background(), nil)
	kv.AssertNotCalled(t, "Put", mock.Anything, mock.Anything, mock.Anything)

	nilCache := CacheLayer{}
	nilCache.invalidate(context.Background(), []invalidationPair{{object: "project:1", relation: "viewer"}})
}

func TestInvalidationLookupMemoizesPerPair(t *testing.T) {
	kv := new(MockNatsKeyValue)
	kv.On("Get", mock.Anything, cachekey.Invalidation("project:1", "viewer")).
		Return(nil, jetstream.ErrKeyNotFound).Once()
	kv.On("Get", mock.Anything, cachekey.Invalidation("project:1", "*")).
		Return(nil, jetstream.ErrKeyNotFound).Once()
	kv.On("Get", mock.Anything, cachekey.Invalidation("project:*", "viewer")).
		Return(nil, jetstream.ErrKeyNotFound).Once()
	kv.On("Get", mock.Anything, legacyInvalidationKey).
		Return(nil, jetstream.ErrKeyNotFound).Once()
	cache := CacheLayer{bucket: kv}
	lookup := newInvalidationLookup(cache)

	first := lookup.get(context.Background(), "project:1", "viewer")
	second := lookup.get(context.Background(), "project:1", "viewer")

	assert.Equal(t, first, second)
	// Four distinct markers are consulted on the first call (the
	// object-scoped literal marker, the object-scoped wildcard-relation
	// marker, the type-wide marker scoped to this relation, and the
	// once-per-batch legacy global marker); all four are memoized (the
	// legacy one via sync.Once), so the second get() call hits the memo for
	// every one of them rather than re-fetching from the KV.
	kv.AssertNumberOfCalls(t, "Get", 4)
}

// TestInvalidationLookupTreatsErrorAsForced is the regression test for the
// clock-dependent staleness hazard: a KV lookup error must force the entry
// to be treated as stale via an explicit flag (invalidationOutcome.forced),
// never via a time.Now() timestamp compared against the cached entry's
// JetStream-server-clock entry.Created(). A wall-clock fallback can be
// silently "fresh enough" if the JetStream server's clock runs ahead of
// this pod's, which would wrongly accept a cache entry despite the failed
// lookup. See invalidationOutcome's doc comment.
func TestInvalidationLookupTreatsErrorAsForced(t *testing.T) {
	kv := new(MockNatsKeyValue)
	kv.On("Get", mock.Anything, cachekey.Invalidation("project:1", "viewer")).
		Return(nil, assert.AnError)
	kv.On("Get", mock.Anything, cachekey.Invalidation("project:1", "*")).
		Return(nil, assert.AnError)
	kv.On("Get", mock.Anything, cachekey.Invalidation("project:*", "viewer")).
		Return(nil, assert.AnError)
	kv.On("Get", mock.Anything, legacyInvalidationKey).
		Return(nil, assert.AnError)
	cache := CacheLayer{bucket: kv}
	lookup := newInvalidationLookup(cache)

	got := lookup.get(context.Background(), "project:1", "viewer")

	assert.True(t, got.forced, "a lookup error must force the entry stale, independent of any clock comparison")
}

func TestExpandTypeWidePairs(t *testing.T) {
	tests := []struct {
		name     string
		object   string
		relation string
		want     []invalidationPair
	}{
		{
			name:     "leaf type with no dependents gets only its own object-scoped marker",
			object:   "committee_invite:1",
			relation: "viewer",
			want:     []invalidationPair{{object: "committee_invite:1", relation: "*"}},
		},
		{
			// vote's only direct dependent is vote_response, and only its
			// "auditor" relation reads "auditor from vote" — vote_response
			// has no "writer"/"viewer" relation of its own to sweep in, so
			// this is the smallest hand-verifiable cross-type case. "writer"
			// is not a cascading relation for vote (vote has no same-type
			// hierarchy), so no extra type-wide marker for vote itself.
			name:     "vote write blanket-invalidates only vote_response's auditor relation",
			object:   "vote:1",
			relation: "writer",
			want: []invalidationPair{
				{object: "vote:1", relation: "*"},
				{object: "vote_response:*", relation: "auditor"},
			},
		},
		{
			// b2b_org -> project_membership -> b2b_org is a real cycle
			// (project_membership.auditor reads "auditor from b2b_org";
			// b2b_org.auditor reads "key_contact from membership"), so the
			// closure must terminate rather than loop forever. Unlike an
			// earlier version of this fix, a genuine multi-hop cycle back to
			// the source type is now preserved rather than discarded (see
			// computeTypeInvalidationFanout's doc comment), so
			// typeInvalidationFanout["b2b_org"]["b2b_org"] legitimately
			// contains {"auditor", "key_contact"} here, producing
			// b2b_org:*,auditor and b2b_org:*,key_contact markers. Separately,
			// "global_org_admin" feeds b2b_org's own writer->auditor same-type
			// cascade via cascadingFeeders/cascadingFanout (see
			// expandTypeWidePairs), independently producing b2b_org:*,writer
			// and a second, duplicate b2b_org:*,auditor entry — expandTypeWidePairs
			// does not dedupe across these two independent mechanisms (only
			// CacheLayer.invalidate's "seen" map dedupes, at the caller level),
			// so the duplicate is listed explicitly below rather than papered
			// over. See TestExpandTypeWidePairsCascadesSameTypeHierarchy for
			// the writer/auditor + cascadingRelations interaction in isolation.
			name:     "b2b_org/project_membership cycle resolves to a finite, relation-scoped closure",
			object:   "b2b_org:1",
			relation: "global_org_admin",
			want: []invalidationPair{
				{object: "b2b_org:1", relation: "*"},
				{object: "project_membership:*", relation: "writer"},
				{object: "project_membership:*", relation: "auditor"},
				{object: "crowdfunding_initiative:*", relation: "writer"},
				{object: "crowdfunding_initiative:*", relation: "viewer"},
				{object: "b2b_org:*", relation: "writer"},
				{object: "b2b_org:*", relation: "auditor"},
				{object: "b2b_org:*", relation: "key_contact"},
				{object: "b2b_org:*", relation: "auditor"},
			},
		},
		{
			name:     "unknown type still gets its own object-scoped marker",
			object:   "user:1",
			relation: "member",
			want:     []invalidationPair{{object: "user:1", relation: "*"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := expandTypeWidePairs(tt.object, tt.relation)
			assert.ElementsMatch(t, tt.want, got)
		})
	}
}

// TestExpandTypeWidePairsCascadesSameTypeHierarchy is the regression test
// for the same-type ancestor/descendant cascade gap: writing a cascading
// relation (or a hierarchy edge) on one object of a type must additionally
// bump a type-wide (type:*, relation) marker, because the object-scoped
// marker alone cannot reach a *different* object of the same type that
// cascades from/to the written one (see cascadingRelations,
// hierarchyEdgeRelations).
func TestExpandTypeWidePairsCascadesSameTypeHierarchy(t *testing.T) {
	t.Run("writing a cascading relation bumps its own and every downstream cascading relation's type-wide marker", func(t *testing.T) {
		// owner feeds writer, which in turn feeds auditor (see
		// cascadingFeeders[fgaTypeProject]), so writing owner must bump the
		// type-wide marker for all three: a change to owner can change what
		// "writer from owner" and "auditor from writer" resolve to for other
		// project objects that inherit from this one.
		got := expandTypeWidePairs("project:123", "owner")
		assert.Contains(t, got, invalidationPair{object: "project:*", relation: "owner"})
		assert.Contains(t, got, invalidationPair{object: "project:*", relation: "writer"},
			"writing owner must also bump writer's type-wide marker, since writer cascades from owner")
		assert.Contains(t, got, invalidationPair{object: "project:*", relation: "auditor"},
			"writing owner must transitively bump auditor's type-wide marker, since auditor cascades from writer which cascades from owner")
	})

	t.Run("writing the parent hierarchy edge bumps every cascading relation's type-wide marker", func(t *testing.T) {
		got := expandTypeWidePairs("project:123", "parent")
		for _, r := range []string{"owner", "writer", "auditor", "marketing_ops", "marketing_auditor"} {
			assert.Contains(t, got, invalidationPair{object: "project:*", relation: r},
				"re-parenting project:123 must invalidate every cascading relation, since any of them could now resolve differently")
		}
	})

	t.Run("writing the parent hierarchy edge also bumps every derived relation fed by a cascading relation", func(t *testing.T) {
		// Reparenting project:123 changes what owner/writer/auditor/
		// marketing_ops resolve to for project:123's descendants, which
		// changes those descendants' writer_guard/auditor_guard/viewer/
		// meetings_creator/mentorship_program_creator/campaign_manager too,
		// since each reads a cascading relation same-object (see
		// cascadingFeeders). Regression test for the gap where this branch
		// bumped only the base cascading relations, not their fanout.
		got := expandTypeWidePairs("project:123", "parent")
		for _, r := range []string{
			relWriterGuard, relAuditorGuard, constants.RelationViewer,
			relMeetingsCreator, relMentorshipProgramCreator, relCampaignManager,
		} {
			assert.Contains(t, got, invalidationPair{object: "project:*", relation: r},
				"re-parenting project:123 must invalidate %q, which is fed by a cascading relation", r)
		}
	})

	t.Run("b2b_org writer/auditor cascade via both parent and child edges", func(t *testing.T) {
		got := expandTypeWidePairs("b2b_org:1", "child")
		assert.Contains(t, got, invalidationPair{object: "b2b_org:*", relation: "writer"})
		assert.Contains(t, got, invalidationPair{object: "b2b_org:*", relation: "auditor"})
	})

	t.Run("non-cascading relation gets no type-wide same-type marker", func(t *testing.T) {
		got := expandTypeWidePairs("project:123", "meeting_coordinator")
		assert.NotContains(t, got, invalidationPair{object: "project:*", relation: "meeting_coordinator"})
	})
}

// TestCascadingFanoutReachesDerivedGuardRelations is the regression test for
// the missing-feeder-destination gap: project's writer_guard/auditor_guard/
// viewer/meetings_creator/mentorship_program_creator/campaign_manager all
// read a cascading relation same-object in model.fga —
//
//	writer_guard: writer or global_writer
//	auditor_guard: auditor or global_auditor
//	viewer: [user:*] or auditor_guard or meeting_coordinator
//	meetings_creator: writer_guard or meeting_coordinator
//	mentorship_program_creator: writer_guard or mentorship_program_admin
//	campaign_manager: executive_director or marketing_ops or global_marketing_ops
//
// — but are intentionally absent from cascadingRelations (they don't
// themselves compose "from parent"). Before cascadingFeeders[fgaTypeProject]
// listed them as destinations, none of these six relations ever got a
// type-wide (project:*, relation) marker, so a write to an ancestor
// project's owner/writer/auditor never invalidated a descendant's cached
// writer_guard/auditor_guard/viewer/meetings_creator/
// mentorship_program_creator check, and a write to executive_director never
// invalidated a descendant's cached auditor/marketing_auditor check (see
// also TestExpandTypeWidePairsCascadesSameTypeHierarchy for the
// owner-\>writer-\>auditor case already covered before this fix).
//
// campaign_manager is deliberately NOT fed by executive_director or
// global_marketing_ops, even though both compose it same-object in
// model.fga: neither cascades ("from parent"), so a write to either can
// only ever change campaign_manager on the SAME object — already covered
// by the unconditional object-scoped wildcardRelation marker — and listing
// them here would bump the type-wide (project:*, campaign_manager) marker
// for every project in the store for no cross-object benefit. Only
// marketing_ops (which does cascade) is listed as a campaign_manager
// source.
func TestCascadingFanoutReachesDerivedGuardRelations(t *testing.T) {
	fanout := cascadingFanout[fgaTypeProject]

	t.Run("owner transitively reaches every guard/derived relation it feeds, but not campaign_manager", func(t *testing.T) {
		for _, r := range []string{
			"writer", "auditor", "writer_guard", "auditor_guard",
			"viewer", "meetings_creator", "mentorship_program_creator",
		} {
			assert.True(t, fanout["owner"][r],
				"owner must transitively reach %q: owner -> writer -> {auditor, writer_guard} -> {auditor_guard, meetings_creator, mentorship_program_creator} -> viewer", r)
		}
		assert.False(t, fanout["owner"]["campaign_manager"],
			"campaign_manager is only fed by executive_director/marketing_ops/global_marketing_ops, none of which owner reaches")
	})

	t.Run("executive_director feeds auditor and marketing_auditor directly, plus auditor_guard/viewer transitively, but not campaign_manager", func(t *testing.T) {
		for _, r := range []string{
			constants.RelationAuditor, relMarketingAuditor,
			"auditor_guard", constants.RelationViewer,
		} {
			assert.True(t, fanout[constants.RelationExecutiveDirector][r],
				"executive_director must reach %q", r)
		}
		assert.False(t, fanout[constants.RelationExecutiveDirector]["campaign_manager"],
			"executive_director does not cascade (plain [user] grant), so writing it can only change campaign_manager on the SAME object — already covered by the object-scoped marker, not a type-wide one")
	})

	t.Run("global_marketing_ops feeds marketing_auditor but not campaign_manager", func(t *testing.T) {
		assert.True(t, fanout[relGlobalMarketingOps][relMarketingAuditor], "global_marketing_ops must feed marketing_auditor")
		assert.False(t, fanout[relGlobalMarketingOps]["campaign_manager"],
			"global_marketing_ops does not cascade (plain [team#member] grant), so writing it can only change campaign_manager on the SAME object")
	})

	t.Run("marketing_ops feeds both marketing_auditor and campaign_manager, since marketing_ops itself cascades", func(t *testing.T) {
		assert.True(t, fanout[relMarketingOps][relMarketingAuditor], "marketing_ops must feed marketing_auditor")
		assert.True(t, fanout[relMarketingOps]["campaign_manager"],
			"marketing_ops cascades (\"marketing_ops from parent\"), so a descendant's campaign_manager (which reads the descendant's own marketing_ops, itself inherited from this object) can change too")
	})
}

// TestExpandTypeWidePairsCrossTypeIrrelevant is the regression test for
// crossTypeIrrelevant: a write to a relation with no possible path into any
// cross-type dependent's guard must not bump that dependent's type-wide
// marker, even though project has entries in typeInvalidationFanout for
// other relations.
func TestExpandTypeWidePairsCrossTypeIrrelevant(t *testing.T) {
	tests := []struct {
		relation string
		want     []invalidationPair
	}{
		{
			// marketing_ops itself cascades same-type via "marketing_ops
			// from parent" (see cascadingRelations), so it still gets a
			// same-type project:* marker — but never a cross-type one for
			// committee/meeting/etc., since no dependent reads it. It also
			// feeds marketing_auditor AND campaign_manager via
			// cascadingFeeders ("campaign_manager: executive_director or
			// marketing_ops or global_marketing_ops"), so both downstream
			// relations' type-wide markers are bumped too.
			relation: "marketing_ops",
			want: []invalidationPair{
				{object: "project:123", relation: "*"},
				{object: "project:*", relation: "marketing_ops"},
				{object: "project:*", relation: "marketing_auditor"},
				{object: "project:*", relation: "campaign_manager"},
			},
		},
		{
			// global_marketing_ops does not itself cascade same-type (only
			// marketing_auditor, which composes it, does — and that's
			// covered by the object-scoped marker), and no cross-type
			// dependent reads it either. It does feed marketing_auditor via
			// cascadingFeeders, so that relation's type-wide marker is
			// bumped alongside the object-scoped marker — but NOT
			// campaign_manager: global_marketing_ops is a plain
			// [team#member] grant with no "from parent", so writing it can
			// only ever change campaign_manager on the SAME object, which
			// the object-scoped marker above already covers.
			relation: "global_marketing_ops",
			want: []invalidationPair{
				{object: "project:123", relation: "*"},
				{object: "project:*", relation: "marketing_auditor"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.relation, func(t *testing.T) {
			got := expandTypeWidePairs("project:123", tt.relation)
			assert.ElementsMatch(t, tt.want, got,
				"project.%s never feeds a cross-type dependent's guard", tt.relation)
		})
	}
}

// TestExpandTypeWidePairsIsRelationScoped is the direct regression test for
// the type-wide fanout being relation-aware rather than a blanket-per-type
// "*" marker (see PR/issue #2358): a write to project:123 must invalidate
// committee's project-derived relations, but must never invalidate
// committee's "member" relation (a plain user grant with nothing to do with
// project), nor write a blanket all-relations marker for committee at all.
func TestExpandTypeWidePairsIsRelationScoped(t *testing.T) {
	pairs := expandTypeWidePairs("project:123", "writer")

	assert.Contains(t, pairs, invalidationPair{object: "committee:*", relation: "writer"})
	assert.Contains(t, pairs, invalidationPair{object: "committee:*", relation: "auditor"})
	assert.NotContains(t, pairs, invalidationPair{object: "committee:*", relation: "member"},
		"committee.member never reads project; a project write must not stale it")
	assert.NotContains(t, pairs, invalidationPair{object: "committee:*", relation: "*"},
		"project's fanout must never write a blanket all-relations marker for committee")
}

func TestComputeTypeInvalidationFanoutReachesProjectTransitively(t *testing.T) {
	fanout := computeTypeInvalidationFanout(crossTypeDependents)

	// mentorship_task depends on mentorship_application, which depends on
	// mentorship_program, which depends on project: a three-hop chain that
	// only resolves correctly if the closure keeps iterating past one pass.
	assert.True(t, fanout["project"]["mentorship_task"]["auditor"],
		"project's fanout must transitively reach mentorship_task's auditor relation through mentorship_program and mentorship_application")
	assert.True(t, fanout["project"]["committee_invite"]["viewer"],
		"project's fanout must transitively reach committee_invite's viewer relation through committee")

	// Direct userset edges (team#member / mentorship_approver_team#member):
	// these types are never referenced via "from <field>" in model.fga, only
	// directly as "[team#member]"/"[mentorship_approver_team#member]", so a
	// revoked team membership must still reach every relation that reads it.
	assert.True(t, fanout["team"]["project"]["global_owner"],
		"team's fanout must reach project's global_owner relation (reads team#member directly)")
	assert.True(t, fanout["team"]["committee"]["auditor"],
		"team's fanout must reach committee's auditor relation (reads team#member directly)")
	assert.True(t, fanout["team"]["b2b_org"]["auditor"],
		"team's fanout must reach b2b_org's auditor relation (reads team#member directly)")
	assert.True(t, fanout["team"]["project_application"]["formation_team"],
		"team's fanout must reach project_application's formation_team relation (reads team#member directly)")
	assert.True(t, fanout["mentorship_approver_team"]["mentorship_program"]["global_mentorship_approver"],
		"mentorship_approver_team's fanout must reach mentorship_program's global_mentorship_approver relation (reads mentorship_approver_team#member directly)")

	// Relation scoping (see PR/issue #2358 — unrelated writes must not
	// invalidate untouched results): a type showing up in another type's
	// fanout does not mean EVERY one of its relations is swept in, only the
	// ones that actually read the source.
	assert.False(t, fanout["project"]["committee"]["member"],
		"project's fanout must not include committee's member relation, which never reads project")
}

// TestComputeTypeInvalidationFanoutPreservesMultiHopCycle is the regression
// test for the cycle-discarding bug: project_membership.auditor reads
// "auditor from b2b_org" and b2b_org.auditor reads "key_contact from
// membership", a genuine two-hop cycle from project_membership back to
// itself via b2b_org. An earlier version of computeTypeInvalidationFanout's
// fixed-point loop discarded any edge that returned to the source type
// (treating "dd == t" the same as the already-handled direct "d == t"
// no-op), which silently dropped this self-entry, so a b2b_org-mediated
// change never emitted the stale project_membership:* marker it should
// have. A self-entry produced by a real intermediate hop (b2b_org) must be
// kept.
func TestComputeTypeInvalidationFanoutPreservesMultiHopCycle(t *testing.T) {
	fanout := computeTypeInvalidationFanout(crossTypeDependents)

	assert.True(t, fanout["project_membership"]["project_membership"]["auditor"],
		"project_membership's fanout must include its own auditor relation, reached via the real "+
			"project_membership -> b2b_org -> project_membership cycle (b2b_org.auditor reads "+
			"key_contact from membership; project_membership.auditor reads auditor from b2b_org)")

	// The same cycle, exercised end-to-end through expandTypeWidePairs: a
	// write to b2b_org's key_contact relation must still emit a type-wide
	// marker for project_membership:*,auditor, closing the loop rather than
	// silently dropping it.
	got := expandTypeWidePairs("b2b_org:1", "key_contact")
	assert.Contains(t, got, invalidationPair{object: "project_membership:*", relation: "auditor"},
		"writing b2b_org's key_contact relation must still bump project_membership's type-wide auditor marker "+
			"despite the cycle back to b2b_org's own type")
}

func TestInvalidationLookupAlwaysConsultsTypeWideWildcardMarker(t *testing.T) {
	kv := new(MockNatsKeyValue)
	objectEntry := &MockKeyValueEntry{created: time.Now().Add(-time.Hour)}
	objectWildcardEntry := &MockKeyValueEntry{created: time.Now().Add(-30 * time.Minute)}
	wildcardEntry := &MockKeyValueEntry{created: time.Now()}
	kv.On("Get", mock.Anything, cachekey.Invalidation("project:1", "writer")).
		Return(objectEntry, nil)
	kv.On("Get", mock.Anything, cachekey.Invalidation("project:1", "*")).
		Return(objectWildcardEntry, nil)
	kv.On("Get", mock.Anything, cachekey.Invalidation("project:*", "writer")).
		Return(wildcardEntry, nil)
	kv.On("Get", mock.Anything, legacyInvalidationKey).
		Return(nil, jetstream.ErrKeyNotFound)
	cache := CacheLayer{bucket: kv}
	lookup := newInvalidationLookup(cache)

	got := lookup.get(context.Background(), "project:1", "writer")

	assert.False(t, got.forced)
	assert.Equal(t, wildcardEntry.created, got.t, "the later blanket marker must win over the object-scoped and object-wildcard ones")
	kv.AssertNumberOfCalls(t, "Get", 4)
}

func TestInvalidationLookupConsultsWildcardMarkerForAnyRelation(t *testing.T) {
	kv := new(MockNatsKeyValue)
	kv.On("Get", mock.Anything, cachekey.Invalidation("project:1", "viewer")).
		Return(nil, jetstream.ErrKeyNotFound)
	kv.On("Get", mock.Anything, cachekey.Invalidation("project:1", "*")).
		Return(nil, jetstream.ErrKeyNotFound)
	kv.On("Get", mock.Anything, cachekey.Invalidation("project:*", "viewer")).
		Return(nil, jetstream.ErrKeyNotFound)
	kv.On("Get", mock.Anything, legacyInvalidationKey).
		Return(nil, jetstream.ErrKeyNotFound)
	cache := CacheLayer{bucket: kv}
	lookup := newInvalidationLookup(cache)

	lookup.get(context.Background(), "project:1", "viewer")

	kv.AssertNumberOfCalls(t, "Get", 4)
	kv.AssertCalled(t, "Get", mock.Anything, cachekey.Invalidation("project:*", "viewer"))
	kv.AssertCalled(t, "Get", mock.Anything, cachekey.Invalidation("project:1", "*"))
}

func TestInvalidationLookupConsultsLegacyGlobalMarkerOnce(t *testing.T) {
	kv := new(MockNatsKeyValue)
	kv.On("Get", mock.Anything, cachekey.Invalidation("project:1", "viewer")).
		Return(nil, jetstream.ErrKeyNotFound)
	kv.On("Get", mock.Anything, cachekey.Invalidation("project:1", "auditor")).
		Return(nil, jetstream.ErrKeyNotFound)
	kv.On("Get", mock.Anything, cachekey.Invalidation("project:1", "*")).
		Return(nil, jetstream.ErrKeyNotFound)
	kv.On("Get", mock.Anything, cachekey.Invalidation("project:*", "viewer")).
		Return(nil, jetstream.ErrKeyNotFound)
	kv.On("Get", mock.Anything, cachekey.Invalidation("project:*", "auditor")).
		Return(nil, jetstream.ErrKeyNotFound)
	legacyEntry := &MockKeyValueEntry{created: time.Now()}
	kv.On("Get", mock.Anything, legacyInvalidationKey).
		Return(legacyEntry, nil).Once()
	cache := CacheLayer{bucket: kv}
	lookup := newInvalidationLookup(cache)

	got := lookup.get(context.Background(), "project:1", "viewer")
	got2 := lookup.get(context.Background(), "project:1", "auditor")

	assert.False(t, got.forced)
	assert.False(t, got2.forced)
	assert.Equal(t, legacyEntry.created, got.t,
		"the legacy global marker must win when it is the latest of the timestamps consulted")
	assert.Equal(t, legacyEntry.created, got2.t,
		"a second get() call for a different relation on the same object must still observe the legacy marker")
	// The legacy key is read exactly once across both get() calls, memoized
	// via sync.Once rather than the per-pair memo map. The object-scoped
	// wildcard-relation marker (project:1, "*") is also shared between the
	// two calls and only fetched once. So: 4 calls for the first get()
	// (object literal, object wildcard, type-wide-for-"viewer", legacy) plus
	// 2 for the second (object literal for "auditor", type-wide-for-
	// "auditor" — the object wildcard and legacy are both already memoized).
	kv.AssertNumberOfCalls(t, "Get", 6)
}

func TestSyncObjectTuplesSeedsPositiveCacheOnlyAfterSuccessfulWrite(t *testing.T) {
	tests := []struct {
		name          string
		writeErr      error
		wantCacheSeed bool
	}{
		{name: "successful OpenFGA write", wantCacheSeed: true},
		{name: "failed OpenFGA write", writeErr: assert.AnError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fgaClient := new(MockFgaClient)
			fgaClient.
				On("Read", mock.Anything, mock.Anything, client.ClientReadOptions{}).
				Return(&client.ClientReadResponse{}, nil)
			fgaClient.
				On("Write", mock.Anything, mock.Anything, mock.Anything).
				Return(&client.ClientWriteResponse{}, tt.writeErr)
			cache := &cacheWriteRecorder{writes: make(chan string, 1)}
			service := newFgaService(fgaClient, cache, false)
			tuple := client.ClientTupleKey{User: "user:alice", Relation: "writer", Object: "project:resource-1"}

			_, _, err := service.SyncObjectTuples(
				context.Background(),
				"project:resource-1",
				[]client.ClientTupleKey{tuple},
			)

			if tt.writeErr != nil {
				require.ErrorIs(t, err, tt.writeErr)
			} else {
				require.NoError(t, err)
			}

			// seedPositiveCacheEntries blocks until all cache writes complete, so by
			// the time SyncObjectTuples returns the write has either already
			// happened or will never happen; no wait is needed either way.
			select {
			case <-cache.writes:
				assert.True(t, tt.wantCacheSeed, "cache was seeded after failed OpenFGA write")
			default:
				assert.False(t, tt.wantCacheSeed, "cache was not seeded after successful OpenFGA write")
			}
		})
	}
}

// TestSyncObjectTuplesDoesNotSeedCacheForTupleSkippedDuringInvalidTupleRetry
// covers the case where writeAndDeleteTuplesBatch removes one OpenFGA-rejected
// tuple and retries successfully with the rest. The overall write succeeds,
// but the removed tuple was never stored, so its cache key must not be seeded
// alongside the tuple that did survive.
func TestSyncObjectTuplesDoesNotSeedCacheForTupleSkippedDuringInvalidTupleRetry(t *testing.T) {
	fgaClient := new(MockFgaClient)
	fgaClient.
		On("Read", mock.Anything, mock.Anything, client.ClientReadOptions{}).
		Return(&client.ClientReadResponse{}, nil)
	// First attempt includes both tuples; OpenFGA rejects alice's writer grant.
	fgaClient.
		On("Write", mock.Anything, mock.MatchedBy(func(req client.ClientWriteRequest) bool {
			return len(req.Writes) == 2
		}), mock.Anything).
		Return((*client.ClientWriteResponse)(nil), makeValidationError(
			"Invalid tuple 'project:resource-1#writer@user:alice'. Reason: relation 'project#writer' not found",
		)).
		Once()
	// Retry with only bob's viewer grant succeeds.
	fgaClient.
		On("Write", mock.Anything, mock.MatchedBy(func(req client.ClientWriteRequest) bool {
			return len(req.Writes) == 1 && req.Writes[0].User == "user:bob"
		}), mock.Anything).
		Return(&client.ClientWriteResponse{}, nil).
		Once()

	cache := &cacheWriteRecorder{writes: make(chan string, 2)}
	service := newFgaService(fgaClient, cache, false)

	writes := []client.ClientTupleKey{
		{User: "user:alice", Relation: "writer", Object: "project:resource-1"},
		{User: "user:bob", Relation: "viewer", Object: "project:resource-1"},
	}

	_, _, err := service.SyncObjectTuples(context.Background(), "project:resource-1", writes)
	require.NoError(t, err)

	survivingCacheKey := cachekey.Entry("project:resource-1#viewer@user:bob")

	// seedPositiveCacheEntries blocks until all cache writes complete, so both
	// checks below are deterministic by the time SyncObjectTuples has returned.
	select {
	case key := <-cache.writes:
		assert.Equal(t, survivingCacheKey, key, "seeded cache key should be for the tuple OpenFGA actually stored")
	default:
		t.Fatal("expected the surviving tuple's cache entry to be seeded")
	}

	select {
	case key := <-cache.writes:
		t.Fatalf("unexpected second cache write for %q; the skipped invalid tuple must not be seeded", key)
	default:
		// No further writes: the skipped tuple's cache key was correctly excluded.
	}
}
