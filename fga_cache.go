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
	"github.com/linuxfoundation/lfx-v2-fga-sync/pkg/constants"
)

// relMarketingOps and relManager name relations that recur across the
// cross-type/cascade dependency tables below but have no shared constant in
// pkg/constants (unlike relations such as "auditor" or "organizer", which
// pkg/constants already names because more than one call site outside this
// file references them too).
const (
	relMarketingOps             = "marketing_ops"
	relManager                  = "manager"
	relMarketingAuditor         = "marketing_auditor"
	relGlobalMarketingOps       = "global_marketing_ops"
	relWriterGuard              = "writer_guard"
	relAuditorGuard             = "auditor_guard"
	relCampaignManager          = "campaign_manager"
	relMeetingsCreator          = "meetings_creator"
	relMentorshipProgramCreator = "mentorship_program_creator"
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

// crossTypeEdge names one dependent type reached from a crossTypeDependents
// source entry, along with the SPECIFIC relations on that dependent type that
// (directly, or via same-type composition) read a relation on the source
// type — via a "<relation> from <field>" reference OR a direct userset
// reference (e.g. "[team#member]") in charts/lfx-platform/files/model.fga.
// Scoping to relations, rather than blanket-invalidating every relation of
// the dependent type, matters for the same reason expandTypeWidePairs scopes
// same-type invalidation to the written object rather than "type:*": a write
// to project:123 must not stale an unrelated committee's cached
// "member"/"viewer:*"-composed result that never reads project at all (see
// PR/issue #2358).
type crossTypeEdge struct {
	typ       string
	relations []string
}

// crossTypeDependents lists, for each OpenFGA object type, the OTHER types
// whose relation definitions read this type, and which of THEIR relations do
// so (a DIRECT edge only; the transitive closure across these edges,
// including relation-set propagation, is computed once at init by
// typeInvalidationFanout below). For example committee.writer is defined as
// "writer_guard from project", so committee depends on project for its
// "writer" relation (and everything that composes on top of writer:
// "auditor", "inviter", "viewer", ...), hence
// "project": {{typ: "committee", relations: {"writer", "auditor", ...}}}.
// Likewise committee.auditor includes "[..., team#member]" directly, so
// committee also depends on "team" for "auditor" (and its composed
// relations).
//
// Same-type composition (project.writer building on project.owner,
// b2b_org.writer cascading through parent/child) needs no entry here: every
// write's object-scoped wildcard-relation marker (see expandTypeWidePairs)
// already invalidates every relation of that same object, so intra-type
// chains are covered without enumerating them.
//
// Keep this in sync with model.fga (owned by lfx-v2-helm): add/update an
// entry here whenever a relation definition gains or loses a
// "from <other type>" reference or a "[<other type>#<relation>]" direct
// userset reference, and re-trace which of the dependent type's OWN
// relations transitively read that reference. Getting this wrong in this
// security-sensitive cache is asymmetric: an over-inclusive relation list
// only costs extra cache misses, an under-inclusive one (missing an edge, or
// missing a relation that composes over one) reintroduces the fail-open
// staleness this map exists to close — when in doubt, include the relation.
// Type name constants for the entries below that recur across multiple map
// values (a type can be a dependent of more than one other type).
const (
	fgaTypeMeeting           = "meeting"
	fgaTypePastMeeting       = "past_meeting"
	fgaTypeProjectMembership = "project_membership"
	fgaTypeSurvey            = "survey"
	fgaTypeV1Meeting         = "v1_meeting"
	fgaTypeVote              = "vote"
	fgaTypeProject           = "project"
	fgaTypeCommittee         = "committee"
	fgaTypeB2BOrg            = "b2b_org"
	fgaTypeMentorshipProgram = "mentorship_program"
)

// cascadingRelations lists, per object type, the WRITABLE relations that
// compose "X from parent" (or "from child") in
// charts/lfx-platform/files/model.fga: project's
// owner/writer/auditor/marketing_ops/marketing_auditor cascade down the
// project hierarchy via "... from parent"; b2b_org's writer/auditor cascade
// both up and down via "... from parent"/"... from child". A write to one of
// these relations on object O does not only change whether O's own relation
// holds — because it cascades, it can also change the *evaluated* result of
// a check on any descendant (or, for b2b_org, ancestor) object, even though
// that other object's own tuples were never written. The object-scoped
// wildcard marker every write gets (see expandTypeWidePairs) cannot express
// "and every descendant/ancestor of O too", since that set isn't known
// without a hierarchy walk, so these relations additionally get a
// type-wide (type:*, relation) marker, trading extra cache misses for
// correctness. See also hierarchyEdgeRelations for the parent/child edges
// these cascades traverse.
//
// This mirrors and restores the pre-crossTypeDependents cascadingRelations
// mechanism (see git history around commit 151c4aa): the
// crossTypeDependents/typeInvalidationFanout rework that replaced it only
// ever bumps a marker for the written object itself (or a cross-type
// dependent's OWN type), never for a *different* object of the SAME type
// that the written object cascades to via parent/child. Without this map,
// writing project:123's owner tuple no longer invalidated project:456 (a
// descendant of 123)'s cached writer_guard-derived checks — a regression
// against the cascade-closing behavior this PR's description commits to.
// Keep in sync with model.fga.
var cascadingRelations = map[string]map[string]bool{
	fgaTypeProject: {
		constants.RelationOwner: true, constants.RelationWriter: true, constants.RelationAuditor: true,
		relMarketingOps: true, relMarketingAuditor: true,
	},
	fgaTypeB2BOrg: {constants.RelationWriter: true, constants.RelationAuditor: true},
}

// hierarchyEdgeRelations names the relation(s), on the same object types
// listed in cascadingRelations, whose tuple values define the parent/child
// edges those cascades traverse (project.parent; b2b_org.parent and
// b2b_org.child). Writing or deleting one of these edge tuples changes
// which objects a cascading relation reaches without writing that relation
// directly, so it must trigger the same type-wide invalidation as a direct
// write to one of that type's cascading relations — for every relation in
// cascadingRelations[typ], since re-parenting can change any of them.
var hierarchyEdgeRelations = map[string]bool{"parent": true, "child": true}

// cascadingFeeders lists, for types in cascadingRelations, DIRECT same-object
// relation-to-relation edges from model.fga: writing the source relation
// (map key) on an object changes the EVALUATED value of every destination
// relation (map value) on that SAME object, via a same-object "or
// <relation>"/"[<type>#<relation>]" reference — not the parent/child
// hierarchy edge itself (see hierarchyEdgeRelations for that). For example
// project.writer is defined as "[user] or owner or writer from parent", so
// writing project:X's owner changes project:X's own evaluated writer (and
// transitively auditor, since writer feeds auditor) even though writer's
// tuple was never written. The source relation need not itself be a
// cascading relation: b2b_org.owner and b2b_org.global_org_admin are plain
// [user]/[team#member] grants with no "from parent"/"from child" of their
// own, but they feed directly into b2b_org.writer, which does cascade — so
// a write to b2b_org:X's owner can, via X's now-changed writer, also change
// a DIFFERENT b2b_org object's cached writer/auditor check reached through
// "writer from parent"/"writer from child". Because the destination
// relation(s) reached (directly or transitively, see cascadingFanout below)
// are cascading, the object-scoped wildcardRelation marker (scoped to the
// written object only) cannot express that cross-object effect, so
// expandTypeWidePairs additionally emits a type-wide (type:*, relation)
// marker for every relation in cascadingFanout[typ][relation].
//
// The destination relation need not itself be cascading either, so long as
// it transitively reaches one: project.writer_guard ("writer or
// global_writer") and project.auditor_guard ("auditor or global_auditor")
// are same-object reads of writer/auditor but do not themselves compose
// "from parent", so they are correctly absent from cascadingRelations — yet
// project.viewer, project.meetings_creator, and
// project.mentorship_program_creator all read writer_guard/auditor_guard,
// and project.campaign_manager reads executive_director/marketing_ops/
// global_marketing_ops directly. Without listing writer_guard/auditor_guard
// (and executive_director/marketing_ops/global_marketing_ops for
// campaign_manager) as sources here, a write to project:123's owner/writer/
// auditor/marketing_ops/executive_director would never bump the type-wide
// marker for any of these six relations, so a cached check for
// project:456#viewer (a descendant of 123) would stay "fresh" despite its
// evaluated value having changed.
//
// Keep in sync with model.fga alongside cascadingRelations/
// hierarchyEdgeRelations: add an entry whenever a relation gains a
// same-object reference into a relation listed in cascadingRelations, OR
// into a relation that itself (directly or transitively) reads one.
var cascadingFeeders = map[string]map[string][]string{
	fgaTypeProject: {
		"global_owner":                      {constants.RelationOwner},
		constants.RelationOwner:             {constants.RelationWriter},
		constants.RelationWriter:            {constants.RelationAuditor, relWriterGuard},
		constants.RelationAuditor:           {relAuditorGuard},
		relWriterGuard:                      {relMeetingsCreator, relMentorshipProgramCreator},
		relAuditorGuard:                     {constants.RelationViewer},
		constants.RelationExecutiveDirector: {constants.RelationAuditor, relMarketingAuditor, relCampaignManager},
		relGlobalMarketingOps:               {relMarketingAuditor, relCampaignManager},
		relMarketingOps:                     {relMarketingAuditor, relCampaignManager},
	},
	fgaTypeB2BOrg: {
		constants.RelationOwner:  {constants.RelationWriter},
		"global_org_admin":       {constants.RelationWriter},
		constants.RelationWriter: {constants.RelationAuditor},
	},
}

// cascadingFanout is the transitive closure of cascadingFeeders, computed
// once at init the same way typeInvalidationFanout closes
// crossTypeDependents: cascadingFanout[typ][relation] is the full set of
// cascading relations (see cascadingRelations) whose type-wide (type:*,
// relation) marker must additionally be written when relation is written on
// an object of type typ, so that a single "owner" write on project fans out
// to both "writer" and "auditor", not just the one directly-fed relation.
var cascadingFanout = computeCascadingFanout(cascadingFeeders)

func computeCascadingFanout(direct map[string]map[string][]string) map[string]map[string]map[string]bool {
	fanout := make(map[string]map[string]map[string]bool, len(direct))
	for typ, edges := range direct {
		set := make(map[string]map[string]bool, len(edges))
		for src, targets := range edges {
			s := set[src]
			if s == nil {
				s = make(map[string]bool, len(targets))
				set[src] = s
			}
			for _, d := range targets {
				s[d] = true
			}
		}
		for changed := true; changed; {
			changed = false
			for _, targets := range set {
				var extra []string
				for d := range targets {
					more, ok := set[d]
					if !ok {
						continue
					}
					for dd := range more {
						if !targets[dd] {
							extra = append(extra, dd)
						}
					}
				}
				for _, e := range extra {
					if !targets[e] {
						targets[e] = true
						changed = true
					}
				}
			}
		}
		fanout[typ] = set
	}
	return fanout
}

// crossTypeIrrelevant lists, per source type, WRITABLE relations verified
// against model.fga to never feed any cross-type dependent's "from <type>"
// reference or "[<type>#relation]" direct userset reference, directly or
// via same-type composition — so a write to one of them cannot change any
// cross-type dependent's evaluated result, and the type-wide fanout from
// typeInvalidationFanout can be safely skipped for it (the object-scoped
// marker from expandTypeWidePairs still fires regardless, covering the
// written object's own same-type composition).
//
// This is a narrow, explicitly-verified exclusion list, not a general
// "only fire for what's needed" allowlist: getting this wrong in the
// exclusionary direction reintroduces the fail-open staleness
// crossTypeDependents exists to close, so an entry is only added here after
// confirming (by reading every "from <type>"/"[<type>#...]" reference in
// model.fga) that the relation has no path into any dependent's guard.
// Currently audited for project only — every writable relation on the
// other source types in crossTypeDependents does feed some dependent, so
// their type-wide fanout keeps firing unconditionally per the
// asymmetric-cost tradeoff documented on crossTypeDependents.
//
// project.marketing_ops and project.global_marketing_ops are excluded: no
// dependent type reads either directly or transitively (they only feed
// project's own marketing_auditor/campaign_manager, both same-object,
// already covered by the object-scoped marker). Every other writable
// project relation (owner, writer, auditor, global_owner, global_writer,
// global_auditor, meeting_coordinator, mentorship_program_admin,
// executive_director, parent) feeds writer_guard, auditor_guard, or a
// direct "from project" reference used by at least one dependent, so none
// of those are excluded.
var crossTypeIrrelevant = map[string]map[string]bool{
	fgaTypeProject: {relMarketingOps: true, relGlobalMarketingOps: true},
}

// Relation lists reused across several crossTypeDependents entries below,
// because "meeting"-shaped and "vote/survey"-shaped types share the same
// composition pattern.
var (
	meetingLikeRelations = []string{
		constants.RelationAuditor, constants.RelationOrganizer, "host", "participant", constants.RelationViewer,
	}
	pastMeetingRelations = []string{constants.RelationAuditor, constants.RelationOrganizer, constants.RelationViewer}
	voteSurveyRelations  = []string{
		constants.RelationWriter, constants.RelationAuditor, constants.RelationViewer, "results_viewer",
	}
	attachmentRelations = []string{
		constants.RelationWriter, constants.RelationAuditor, "participant", constants.RelationViewer,
	}
	groupsioListRelations = []string{constants.RelationWriter, constants.RelationAuditor, constants.RelationViewer}
)

var crossTypeDependents = map[string][]crossTypeEdge{
	fgaTypeProject: {
		{typ: fgaTypeMentorshipProgram, relations: []string{
			constants.RelationWriter, relManager, constants.RelationAuditor, constants.RelationViewer,
		}},
		{typ: fgaTypeCommittee, relations: []string{
			constants.RelationWriter, constants.RelationAuditor, "inviter",
			constants.RelationViewer, "roster_viewer", "email_viewer",
		}},
		{typ: "groupsio_service", relations: groupsioListRelations},
		{typ: fgaTypeMeeting, relations: meetingLikeRelations},
		{typ: fgaTypePastMeeting, relations: pastMeetingRelations},
		{typ: fgaTypeV1Meeting, relations: meetingLikeRelations},
		{typ: "v1_past_meeting", relations: []string{
			constants.RelationAuditor, constants.RelationOrganizer, constants.RelationViewer,
			"recording_viewer", "transcript_viewer", "ai_summary_viewer",
		}},
		{typ: fgaTypeVote, relations: voteSurveyRelations},
		{typ: fgaTypeSurvey, relations: voteSurveyRelations},
		{typ: fgaTypeProjectMembership, relations: []string{constants.RelationAuditor}},
		{typ: "crowdfunding_initiative", relations: []string{constants.RelationWriter, constants.RelationViewer}},
	},
	fgaTypeMentorshipProgram: {
		{typ: "mentorship_application", relations: []string{
			relManager, constants.RelationWriter, "reviewer", constants.RelationAuditor,
		}},
	},
	"mentorship_application": {
		{typ: "mentorship_task", relations: []string{relManager, constants.RelationAuditor}},
	},
	fgaTypeCommittee: {
		{typ: "committee_invite", relations: []string{constants.RelationViewer}},
		{typ: "groupsio_mailing_list", relations: groupsioListRelations},
		{typ: fgaTypeMeeting, relations: meetingLikeRelations},
		{typ: fgaTypeV1Meeting, relations: meetingLikeRelations},
		{typ: fgaTypeVote, relations: voteSurveyRelations},
		{typ: fgaTypeSurvey, relations: voteSurveyRelations},
	},
	"groupsio_service": {
		{typ: "groupsio_mailing_list", relations: groupsioListRelations},
	},
	fgaTypeMeeting: {
		{typ: "meeting_attachment", relations: attachmentRelations},
		{typ: fgaTypePastMeeting, relations: pastMeetingRelations},
	},
	fgaTypePastMeeting: {
		{typ: "past_meeting_attachment", relations: attachmentRelations},
	},
	fgaTypeV1Meeting: {
		{typ: "v1_past_meeting", relations: []string{
			constants.RelationAuditor, constants.RelationOrganizer, constants.RelationViewer,
			"recording_viewer", "transcript_viewer", "ai_summary_viewer",
		}},
	},
	fgaTypeVote: {
		{typ: "vote_response", relations: []string{constants.RelationAuditor}},
	},
	fgaTypeSurvey: {
		{typ: "survey_response", relations: []string{constants.RelationAuditor}},
	},
	fgaTypeB2BOrg: {
		{typ: fgaTypeProjectMembership, relations: []string{constants.RelationWriter, constants.RelationAuditor}},
		{typ: "crowdfunding_initiative", relations: []string{constants.RelationWriter, constants.RelationViewer}},
	},
	fgaTypeProjectMembership: {
		{typ: fgaTypeB2BOrg, relations: []string{"key_contact", constants.RelationAuditor}},
	},
	// Direct userset edges: these types are never read via "from <field>",
	// only referenced directly as "[team#member]" /
	// "[mentorship_approver_team#member]" in the relations listed below, so a
	// write to team:X#member or mentorship_approver_team:X#member must
	// invalidate those specific dependent relations the same way a "from"
	// edge would.
	"team": {
		// project.global_owner/global_writer/global_auditor/global_marketing_ops,
		// project.owner/auditor/marketing_ops/marketing_auditor all read
		// [team#member] directly; the rest of this list is what composes on
		// top of them (writer_guard, auditor_guard, viewer,
		// meetings_creator, campaign_manager, mentorship_program_creator).
		// Excluded as unaffected: parent, executive_director,
		// meeting_coordinator, mentorship_program_admin (all static [user]
		// grants unrelated to team).
		{typ: fgaTypeProject, relations: []string{
			"global_owner", "global_writer", "global_auditor", relGlobalMarketingOps,
			"owner", constants.RelationWriter, relWriterGuard, constants.RelationAuditor, relAuditorGuard,
			relMarketingOps, relMarketingAuditor, relCampaignManager,
			constants.RelationViewer, relMeetingsCreator, relMentorshipProgramCreator,
		}},
		// committee.auditor reads [team#member] directly.
		{typ: fgaTypeCommittee, relations: []string{
			constants.RelationAuditor, "inviter", constants.RelationViewer, "roster_viewer", "email_viewer",
		}},
		// b2b_org.global_org_admin and b2b_org.auditor read [team#member]
		// directly; global_org_admin also feeds writer.
		{typ: fgaTypeB2BOrg, relations: []string{"global_org_admin", constants.RelationWriter, constants.RelationAuditor}},
		// project_application.formation_team reads [team#member] directly.
		{typ: "project_application", relations: []string{
			"formation_team", constants.RelationViewer, constants.RelationWriter,
		}},
	},
	"mentorship_approver_team": {
		// mentorship_program.global_mentorship_approver reads
		// [mentorship_approver_team#member] directly.
		{typ: fgaTypeMentorshipProgram, relations: []string{
			"global_mentorship_approver", "approver", constants.RelationAuditor, constants.RelationViewer,
		}},
	},
}

// typeInvalidationFanout is the transitive closure of crossTypeDependents,
// computed once at init: typeInvalidationFanout[T] maps every OTHER type D
// reachable from T to the set of D's own relations whose blanket marker must
// be written when a tuple on a T object is written or deleted. Computed via
// fixed-point iteration rather than a naive recursive walk because the graph
// has at least one real cycle (project_membership depends on b2b_org and
// vice versa: model.fga defines b2b_org.auditor as including "key_contact
// from membership" while also defining project_membership.auditor as
// including "auditor from b2b_org"); a memoized depth-first walk would cache
// an incomplete set for whichever member of the cycle it visits first.
//
// Transitive hops are not gated on which of T's relations were written
// (crossTypeDependents entries fire on any write to the source type, per the
// asymmetric-cost tradeoff documented there), so propagating from an
// intermediate type D to a further type E is likewise unconditional: D's
// full relation set (as seen by T) is unioned into T's fanout for E.
//
// The cycle above also means typeInvalidationFanout[T] can, correctly,
// contain T itself: walking project_membership -> b2b_org and back to
// project_membership is a genuine 2-hop cycle, not a no-op self-reference,
// so fanout[project_membership][project_membership] ends up containing
// b2b_org's project_membership-directed relations (writer/auditor). That
// is intentional and required — see the comment inside
// computeTypeInvalidationFanout's fixed-point loop — because a write to one
// project_membership object (M1) can, via the type-wide b2b_org marker it
// bumps, change the evaluated auditor result for a completely different
// project_membership object (M2); only a type-wide project_membership:*
// marker can express that, since this layer has no reverse index from M1 to
// M2.
var typeInvalidationFanout = computeTypeInvalidationFanout(crossTypeDependents)

func computeTypeInvalidationFanout(direct map[string][]crossTypeEdge) map[string]map[string]map[string]bool {
	fanout := make(map[string]map[string]map[string]bool, len(direct))
	ensure := func(t string) map[string]map[string]bool {
		if fanout[t] == nil {
			fanout[t] = make(map[string]map[string]bool)
		}
		return fanout[t]
	}
	// addRelations merges relations into set[typ], returning whether it grew.
	addRelations := func(set map[string]map[string]bool, typ string, relations []string) bool {
		changed := false
		rs, ok := set[typ]
		if !ok {
			rs = make(map[string]bool, len(relations))
			set[typ] = rs
		}
		for _, r := range relations {
			if !rs[r] {
				rs[r] = true
				changed = true
			}
		}
		return changed
	}

	for t, edges := range direct {
		set := ensure(t)
		for _, e := range edges {
			addRelations(set, e.typ, e.relations)
		}
	}

	for changed := true; changed; {
		changed = false
		for t, set := range fanout {
			// Snapshot dependent types since set may grow while we range.
			depTypes := make([]string, 0, len(set))
			for d := range set {
				depTypes = append(depTypes, d)
			}
			for _, d := range depTypes {
				if d == t {
					continue
				}
				for dd, relations := range fanout[d] {
					// dd == t here means the walk returned to the source type
					// through a genuine >=1-hop cycle (d != t was just
					// checked above, so this is never a trivial direct
					// self-edge — crossTypeDependents never lists a type as
					// its own dependent). That is real: project_membership
					// depends on b2b_org (via key_contact/auditor), and
					// b2b_org depends back on project_membership (via
					// writer/auditor: model.fga defines b2b_org.auditor as
					// including "key_contact from membership" and
					// project_membership.auditor as including "auditor from
					// b2b_org"). Writing project_membership:M1's key_contact
					// bumps a type-wide b2b_org:*'s key_contact/auditor
					// marker (every b2b_org, since the specific linked org
					// isn't known here); because that marker is type-wide, it
					// can affect ANY project_membership whose b2b_org
					// reference composes auditor from that org — e.g. a
					// different membership M2 — not just M1. Discarding this
					// edge would leave no type-wide project_membership:*
					// marker for that case, so M2's cached auditor check
					// would stay fresh incorrectly. Do not reintroduce a
					// dd == t skip here.
					relList := make([]string, 0, len(relations))
					for r := range relations {
						relList = append(relList, r)
					}
					if addRelations(set, dd, relList) {
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

// expandTypeWidePairs returns the wildcard-relation invalidation pair(s) that
// must additionally be written for a tuple write/delete on object, on top of
// the object-scoped literal pair every write already gets. It always returns
// at least one pair: an object-scoped wildcard-relation marker (object,
// wildcardRelation), which closes intra-type transitive chains on this
// specific object, e.g. project:123's viewer building on project:123's
// auditor_guard building on project:123's auditor building on project:123's
// writer building on project:123's owner, without needing to enumerate which
// specific relation depends on which. Scoping this marker to object (rather
// than a type-wide "type:*" wildcard) matters: a write to project:123 must
// not invalidate cached checks on the unrelated project:456 (see PR/issue
// #2358 — unrelated writes must not invalidate untouched results).
//
// When relation is a same-type cascading relation, feeds one (directly or
// transitively — see cascadingFeeders/cascadingFanout), or is a
// hierarchy-edge relation (see cascadingRelations/hierarchyEdgeRelations),
// it additionally returns a type-wide (type:*, relation) marker for the
// affected cascading relation(s) of this SAME type, closing the
// ancestor/descendant gap a purely object-scoped marker cannot express. For
// example writing project:123's owner returns markers for project:*'s
// "owner" AND "writer" AND "auditor" (owner feeds writer, writer feeds
// auditor), not just "owner" — otherwise a descendant project's cached
// "writer"/"auditor" check, reached via "writer from parent"/"auditor from
// parent", would stay fresh despite depending on the now-changed owner.
//
// When the written type has OTHER types in typeInvalidationFanout (cross-type
// dependents) and relation is not listed in crossTypeIrrelevant for this
// type, it additionally returns a type-wide marker for each of those,
// closing cross-type chains such as committee.writer depending on
// project.writer_guard. Those markers remain type-wide (not object-scoped)
// because this layer has no reverse index from the written object to the
// specific dependent objects that reference it (e.g. which committees belong
// to project:123) — over-invalidating every OBJECT of the dependent type is
// the safe fallback there. They are, however, scoped to the SPECIFIC
// relations of that dependent type known to read the written type (see
// crossTypeDependents): a write to project:123 bumps committee:*'s "writer"/
// "auditor"/... markers, but never committee:*'s "member" marker, since
// committee.member has nothing to do with project (see PR/issue #2358 —
// unrelated writes must not invalidate untouched results). Gating on
// relation via crossTypeIrrelevant additionally ensures a write to a
// relation with no possible path into any dependent's guard (currently only
// project.marketing_ops/global_marketing_ops) does not bump a wholly
// unrelated type's wildcard either (same #2358 requirement, applied to the
// cross-type case).
func expandTypeWidePairs(object, relation string) []invalidationPair {
	typ := objectType(object)
	if typ == "" {
		return nil
	}

	// Object-scoped wildcard-relation marker: closes same-object intra-type
	// composition chains without touching any other object of this type.
	pairs := []invalidationPair{{object: object, relation: wildcardRelation}}

	if cascading, ok := cascadingRelations[typ]; ok {
		switch {
		case cascading[relation]:
			pairs = append(pairs, invalidationPair{object: typ + ":" + wildcardObject, relation: relation})
			for r := range cascadingFanout[typ][relation] {
				pairs = append(pairs, invalidationPair{object: typ + ":" + wildcardObject, relation: r})
			}
		case hierarchyEdgeRelations[relation]:
			for r := range cascading {
				pairs = append(pairs, invalidationPair{object: typ + ":" + wildcardObject, relation: r})
			}
		default:
			// relation does not itself cascade (e.g. b2b_org's owner and
			// global_org_admin are plain grants), but it may still feed a
			// cascading relation on this same object (see cascadingFeeders):
			// b2b_org.writer includes "... or owner or global_org_admin",
			// and writer cascades. cascadingFanout[typ][relation] is nil
			// (safe to range over) when relation feeds nothing cascading.
			for r := range cascadingFanout[typ][relation] {
				pairs = append(pairs, invalidationPair{object: typ + ":" + wildcardObject, relation: r})
			}
		}
	}

	if irrelevant := crossTypeIrrelevant[typ]; !irrelevant[relation] {
		if deps, ok := typeInvalidationFanout[typ]; ok {
			for depType, relations := range deps {
				for r := range relations {
					pairs = append(pairs, invalidationPair{object: depType + ":" + wildcardObject, relation: r})
				}
			}
		}
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
	// ROLLOUT COMPATIBILITY, REMOVE AFTER FULL ROLLOUT (see
	// cachekey.LegacyInvalidationKey): dual-write the legacy global "inv"
	// marker alongside the scoped markers above. Without this, a pod
	// running old code — which only ever reads the bare "inv" key, never
	// the "inv."-prefixed scoped markers this method writes — would miss
	// every invalidation processed by a pod running this code during a
	// rolling deploy, and could keep serving a cached "true" under its old
	// "rel."-prefixed entries for the full TTL. This accepts a temporary,
	// coarser global miss on old pods (every write now bumps their one
	// shared key, not just the affected pair) in exchange for closing that
	// fail-open window; delete this write once every pod and
	// out-of-process writer (e.g. scripts/bootstrap/member-tiers-callers)
	// is confirmed running post-relinv code.
	g.Go(func() error {
		if slotErr := withCacheOpSlot(gctx, func() {
			if _, err := c.bucket.Put(gctx, cachekey.LegacyInvalidationKey, []byte("1")); err != nil {
				logger.With(errKey, err).ErrorContext(ctx, "failed to write legacy cache invalidation marker")
			}
		}); slotErr != nil {
			logger.With(errKey, slotErr).ErrorContext(ctx, "failed to write legacy cache invalidation marker")
		}
		return nil
	})
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

// legacyInvalidationKey aliases cachekey.LegacyInvalidationKey (see its doc
// for the full rollout-compatibility rationale). This service both writes it
// (CacheLayer.invalidate, alongside the new scoped markers) and reads it
// (getLastLegacyInvalidation/getLegacy below): during a rolling deploy, a
// pod on either side of the rollout — old code that only knows this bare
// key, or new code that writes/reads the "inv."-prefixed per-pair markers —
// must still observe an invalidation the other side processed. Delete this
// alias, the dual-write in CacheLayer.invalidate, getLastLegacyInvalidation,
// and the getLegacy call site in invalidationLookup.get once every pod in
// the fleet AND every out-of-process writer (e.g.
// scripts/bootstrap/member-tiers-callers) is confirmed running post-relinv
// code (nothing can write or need to read this key anymore).
const legacyInvalidationKey = cachekey.LegacyInvalidationKey

// getLastLegacyInvalidation reads the legacy global invalidation marker and
// returns its creation time, or the zero time when it has not been written
// within the TTL window. See legacyInvalidationKey for why this exists.
func (c CacheLayer) getLastLegacyInvalidation(ctx context.Context) (time.Time, error) {
	var lastInvalidation time.Time
	entry, err := c.bucket.Get(ctx, legacyInvalidationKey)
	switch {
	case err == jetstream.ErrKeyNotFound:
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
// invalidationOutcome is the result of a single marker lookup: either a
// timestamp taken from the JetStream server clock (entry.Created(), safe to
// compare against another entry's entry.Created()), or forced=true when the
// lookup itself failed (KV error or an unfulfilled cacheOpSem slot) and no
// server-clock timestamp is available at all.
//
// forced exists because comparing against the application's own wall clock
// (time.Now()) in place of a missing server timestamp is not a safe stand-in:
// entry.Created() on the cached value comes from the JetStream server clock,
// and if that server's clock runs ahead of this pod's wall clock,
// time.Now().After(entry.Created()) can be false even though the lookup that
// was supposed to confirm freshness never actually succeeded. That would
// silently accept a cache entry whose real freshness could not be verified —
// exactly the fail-open case this cache's error handling is meant to close.
// forced sidesteps the comparison entirely: any lookup failure always wins,
// regardless of either clock's value.
type invalidationOutcome struct {
	t      time.Time
	forced bool
}

type invalidationLookup struct {
	cache CacheLayer
	mu    sync.Mutex
	memo  map[invalidationPair]invalidationOutcome
	group singleflight.Group

	// legacyOnce/legacyResult memoize the single global legacy "inv" marker
	// (see legacyInvalidationKey) for this batch: it is one KV key shared by
	// every (object, relation) pair, so unlike the per-pair markers in memo
	// it only ever needs to be read once per batch, not once per pair.
	legacyOnce   sync.Once
	legacyResult invalidationOutcome
}

func newInvalidationLookup(cache CacheLayer) *invalidationLookup {
	return &invalidationLookup{cache: cache, memo: make(map[invalidationPair]invalidationOutcome)}
}

// get returns the last invalidation time for (object, relation): the latest
// of the object-scoped literal marker, the object-scoped wildcard-relation
// marker (closing intra-type composition for this object, see
// expandTypeWidePairs), the type-wide marker for THIS SPECIFIC relation on
// object's type (closing cross-type dependency chains for the relations
// known to depend on another type, see typeInvalidationFanout), and the
// legacy global marker (see legacyInvalidationKey, rollout compatibility).
// Every marker is always consulted, not gated to a hardcoded set of
// relations, so this closes the intra-type, cross-type, and mixed-version
// rollout gaps a narrower scheme would miss.
//
// The type-wide marker is looked up by the caller's own relation, not a
// wildcard: expandTypeWidePairs only ever writes a (type:*, relation) marker
// for relations actually listed in crossTypeDependents for that type, so an
// unrelated relation (e.g. committee:*#member, which never reads project) is
// correctly never treated as stale by a project write.
//
// A KV error or an unfulfilled cacheOpSem slot forces the result
// (invalidationOutcome.forced = true) rather than substituting the
// application's wall clock for the missing server timestamp: the caller
// cannot verify the cached entry's freshness, so the safe default is to fall
// through to OpenFGA rather than trust a cache we could not confirm is
// fresh, and that decision must not depend on how the two clocks happen to
// compare. See invalidationOutcome.
func (l *invalidationLookup) get(ctx context.Context, object, relation string) invalidationOutcome {
	result := l.getOne(ctx, object, relation)

	if or := l.getOne(ctx, object, wildcardRelation); or.forced {
		result.forced = true
	} else if or.t.After(result.t) {
		result.t = or.t
	}

	if typ := objectType(object); typ != "" {
		if wr := l.getOne(ctx, typ+":"+wildcardObject, relation); wr.forced {
			result.forced = true
		} else if wr.t.After(result.t) {
			result.t = wr.t
		}
	}

	if lr := l.getLegacy(ctx); lr.forced {
		result.forced = true
	} else if lr.t.After(result.t) {
		result.t = lr.t
	}

	return result
}

// getLegacy returns the last write time of the legacy global "inv" marker
// (see legacyInvalidationKey), read at most once per batch via sync.Once —
// every (object, relation) pair in a batch shares the same answer, since the
// legacy marker carries no scoping. ROLLOUT COMPATIBILITY, REMOVE AFTER FULL
// ROLLOUT alongside legacyInvalidationKey.
func (l *invalidationLookup) getLegacy(ctx context.Context) invalidationOutcome {
	l.legacyOnce.Do(func() {
		var result invalidationOutcome
		var lookupErr error
		if slotErr := withCacheOpSlot(ctx, func() {
			result.t, lookupErr = l.cache.getLastLegacyInvalidation(ctx)
		}); slotErr != nil {
			result.forced = true
		} else if lookupErr != nil {
			logger.With(errKey, lookupErr).
				ErrorContext(ctx, "legacy cache invalidation lookup error; treating entry as stale")
			result.forced = true
		}
		l.legacyResult = result
	})
	return l.legacyResult
}

// getOne returns the last invalidation time for a single, literal (object,
// relation) marker, memoizing the result within this batch. Concurrent
// callers for the same pair are collapsed onto one KV Get via l.group:
// without this, every goroutine in a batch racing past the memo check
// before the first writer populates it would issue its own duplicate
// round-trip for what is, within one batch, the same read.
func (l *invalidationLookup) getOne(ctx context.Context, object, relation string) invalidationOutcome {
	pair := invalidationPair{object: object, relation: relation}

	l.mu.Lock()
	if result, ok := l.memo[pair]; ok {
		l.mu.Unlock()
		return result
	}
	l.mu.Unlock()

	key := object + "#" + relation
	v, err, _ := l.group.Do(key, func() (any, error) {
		var result invalidationOutcome
		var lookupErr error
		if slotErr := withCacheOpSlot(ctx, func() {
			result.t, lookupErr = l.cache.getLastInvalidation(ctx, object, relation)
		}); slotErr != nil {
			result.forced = true
		} else if lookupErr != nil {
			logger.With(errKey, lookupErr, "object", object, "relation", relation).
				ErrorContext(ctx, "cache invalidation lookup error; treating entry as stale")
			result.forced = true
		}

		l.mu.Lock()
		l.memo[pair] = result
		l.mu.Unlock()
		return result, nil
	})
	if err != nil {
		// fn above never returns a non-nil error; this is unreachable, but
		// treat it the same as any other lookup failure if it ever changes.
		return invalidationOutcome{forced: true}
	}
	result, ok := v.(invalidationOutcome)
	if !ok {
		return invalidationOutcome{forced: true}
	}
	return result
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
	// object+relation invalidation timestamp, skip it. A forced outcome
	// (invalidationOutcome.forced, see its docs) means the invalidation
	// lookup itself failed, so the entry's freshness could not be verified
	// at all — treat it as stale unconditionally, independent of any
	// timestamp comparison, rather than comparing a wall-clock fallback
	// against entry.Created()'s server clock.
	invalidation := invLookup.get(ctx, tuple.Object, tuple.Relation)
	if invalidation.forced || invalidation.t.After(entry.Created()) {
		logger.With(
			"relation_key", relationKey,
			"last_invalidation", invalidation.t,
			"invalidation_forced", invalidation.forced,
			"entry_created", entry.Created(),
			"entry_value", string(entry.Value()),
		).DebugContext(ctx, "cache stale hit")
		cacheStaleHits.Add(1)
		return cacheLookupOutcome{kind: cacheOutcomeStaleHit, needsCheck: true}
	}

	logger.With(
		"relation_key", relationKey,
		"last_invalidation", invalidation.t,
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
