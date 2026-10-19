// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"

	openfga "github.com/openfga/go-sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestModelGraphPlatformClosures pins the closures that matter for the
// production authorization model: for a tuple on type#relation, which object
// types can have their cached checks changed. Each case was derived by hand
// from testdata/model.fga, not copied from program output.
func TestModelGraphPlatformClosures(t *testing.T) {
	g := platformGraph(t)

	tests := []struct {
		name      string
		typ       string
		rel       string
		wantExact []string // when set, the closure must equal this
		contains  []string
		absent    []string
	}{
		{
			// Removing a meeting participant must invalidate the meeting's
			// viewer checks and its attachments (participant from meeting),
			// and nothing unrelated such as votes.
			name:      "meeting participant reaches meeting and attachments only",
			typ:       "meeting",
			rel:       "participant",
			wantExact: []string{"meeting", "meeting_attachment"},
		},
		{
			// vote#participant feeds vote#viewer only.
			name:      "vote participant stays inside vote",
			typ:       "vote",
			rel:       "participant",
			wantExact: []string{"vote"},
		},
		{
			// A committee writer is writer on every type that inherits
			// writer from the committee, transitively.
			name: "committee writer passes through to inheriting types",
			typ:  "committee",
			rel:  "writer",
			contains: []string{
				"committee", "committee_application", "committee_invite", // auditor from committee
				"groupsio_mailing_list",         // writer from committee
				"meeting", "meeting_attachment", // organizer/writer from committee
				"past_meeting", "past_meeting_attachment", // organizer from meeting
				"v1_meeting", "v1_past_meeting",
				"vote", "vote_response", // writer from committee, auditor from vote
				"survey", "survey_response",
			},
			absent: []string{"project", "team", "b2b_org", "mentorship_program", "project_application"},
		},
		{
			// The tupleset relation itself: changing a meeting's committee
			// changes who inherits writer from it, down the meeting chain.
			name:      "meeting committee link reaches the meeting chain",
			typ:       "meeting",
			rel:       "committee",
			wantExact: []string{"meeting", "meeting_attachment", "past_meeting", "past_meeting_attachment"},
		},
		{
			// A survey's result-access link feeds results_viewer only.
			name:      "survey result access link stays inside survey",
			typ:       "survey",
			rel:       "survey_for_participant_result_access",
			wantExact: []string{"survey"},
		},
		{
			// project#parent feeds project#writer via writer from parent, and
			// from there every type inheriting project permissions, three
			// hops deep for mentorship (program -> application -> task).
			name: "project parent link reaches every project-inheriting type",
			typ:  "project",
			rel:  "parent",
			contains: []string{
				"project", "committee", "meeting", "past_meeting", "v1_meeting", "vote", "survey",
				"groupsio_service", "groupsio_mailing_list", "crowdfunding_initiative", "project_membership",
				"mentorship_program", "mentorship_application", "mentorship_task",
			},
			absent: []string{"team", "b2b_org", "project_application", "mentorship_approver_team"},
		},
		{
			name: "project writer reaches the same closure as the parent link",
			typ:  "project",
			rel:  "writer",
			contains: []string{
				"project", "committee", "meeting", "vote", "survey", "mentorship_task", "project_membership",
			},
			absent: []string{"team", "b2b_org", "project_application"},
		},
		{
			// A team membership change reaches every type that grants a
			// permission to team#member, directly or through inheritance.
			name: "team member reaches types granting to team members",
			typ:  "team",
			rel:  "member",
			contains: []string{
				"team", "project", "committee", "b2b_org", "project_application",
				"meeting", "vote", "survey", "crowdfunding_initiative", "project_membership",
			},
			absent: []string{"mentorship_approver_team", "user"},
		},
		{
			// A different team type with a single grant.
			name:      "mentorship approver team reaches only the program",
			typ:       "mentorship_approver_team",
			rel:       "member",
			wantExact: []string{"mentorship_approver_team", "mentorship_program"},
		},
		{
			// b2b_org is recursive (parent/child) and fans out to
			// memberships and crowdfunding initiatives.
			name:      "b2b org parent link follows the recursion",
			typ:       "b2b_org",
			rel:       "parent",
			wantExact: []string{"b2b_org", "crowdfunding_initiative", "project_membership"},
		},
		{
			// key_contact on a membership feeds b2b_org#key_contact and
			// b2b_org#auditor.
			name:      "membership key contact reaches the org",
			typ:       "project_membership",
			rel:       "key_contact",
			wantExact: []string{"b2b_org", "project_membership"},
		},
		{
			// Leaf type with a single self-contained relation set.
			name:      "committee invite invitee stays inside the invite",
			typ:       "committee_invite",
			rel:       "invitee",
			wantExact: []string{"committee_invite"},
		},
		{
			// committee#member feeds roster and email viewers of committees
			// pointing at it through committee_for_member_*_access.
			name:      "committee member reaches committee only",
			typ:       "committee",
			rel:       "member",
			wantExact: []string{"committee"},
		},
		{
			// v1 past meetings read recordings through self-referential
			// links, so a host change stays inside the type.
			name:      "v1 past meeting host stays inside the type",
			typ:       "v1_past_meeting",
			rel:       "host",
			wantExact: []string{"v1_past_meeting"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := g.affectedTypes(tt.typ, tt.rel)
			require.True(t, ok, "%s#%s must be defined", tt.typ, tt.rel)
			if tt.wantExact != nil {
				assert.Equal(t, tt.wantExact, got)
			}
			for _, want := range tt.contains {
				assert.Contains(t, got, want)
			}
			for _, unwanted := range tt.absent {
				assert.NotContains(t, got, unwanted)
			}
		})
	}
}

func TestModelGraphPlatformInvariants(t *testing.T) {
	g := platformGraph(t)
	require.NotEmpty(t, g.defined)

	for node := range g.defined {
		got, ok := g.affectedTypes(node.typ, node.rel)
		require.True(t, ok, "%s#%s", node.typ, node.rel)
		assert.Contains(t, got, node.typ, "closure of %s#%s must include its own type", node.typ, node.rel)
		assert.True(t, sort.StringsAreSorted(got), "closure of %s#%s must be sorted", node.typ, node.rel)
	}

	t.Run("unknown relation and type are not found", func(t *testing.T) {
		for _, c := range [][2]string{
			{"meeting", "no_such_relation"},
			{"no_such_type", "viewer"},
			{"user", "viewer"},
			{"", ""},
		} {
			_, ok := g.affectedTypes(c[0], c[1])
			assert.False(t, ok, "%v", c)
		}
	})

	t.Run("every platform relation is a closure of types the model defines", func(t *testing.T) {
		types := make(map[string]struct{})
		for node := range g.defined {
			types[node.typ] = struct{}{}
		}
		for node := range g.defined {
			got, _ := g.affectedTypes(node.typ, node.rel)
			for _, typ := range got {
				assert.Contains(t, types, typ)
			}
		}
	})
}

// TestModelGraphSyntheticModels covers rewrite shapes the production model
// does not use today (intersection, exclusion, multiple parent types) but
// which the model may adopt later. The graph must stay correct, or fail
// closed, rather than silently dropping dependencies.
func TestModelGraphSyntheticModels(t *testing.T) {
	tests := []struct {
		name string
		dsl  string
		typ  string
		rel  string
		want []string
	}{
		{
			name: "computed userset within a type",
			dsl: `model
  schema 1.1
type user
type doc
  relations
    define owner: [user]
    define viewer: owner
`,
			typ: "doc", rel: "owner",
			want: []string{"doc"},
		},
		{
			name: "tuple to userset inherits from one parent type",
			dsl: `model
  schema 1.1
type user
type folder
  relations
    define viewer: [user]
type doc
  relations
    define parent: [folder]
    define viewer: viewer from parent
`,
			typ: "folder", rel: "viewer",
			want: []string{"doc", "folder"},
		},
		{
			name: "tuple to userset with several parent types",
			dsl: `model
  schema 1.1
type user
type folder
  relations
    define viewer: [user]
type drive
  relations
    define viewer: [user]
type doc
  relations
    define parent: [folder, drive]
    define viewer: viewer from parent
`,
			typ: "drive", rel: "viewer",
			want: []string{"doc", "drive"},
		},
		{
			name: "changing the parent link itself invalidates inherited access",
			dsl: `model
  schema 1.1
type user
type folder
  relations
    define viewer: [user]
type doc
  relations
    define parent: [folder]
    define viewer: viewer from parent
`,
			typ: "doc", rel: "parent",
			want: []string{"doc"},
		},
		{
			name: "parent type that lacks the computed relation contributes nothing",
			dsl: `model
  schema 1.1
type user
type folder
  relations
    define viewer: [user]
type vault
  relations
    define keeper: [user]
type doc
  relations
    define parent: [folder, vault]
    define viewer: viewer from parent
`,
			typ: "vault", rel: "keeper",
			want: []string{"vault"},
		},
		{
			name: "direct userset subject links the group to the object",
			dsl: `model
  schema 1.1
type user
type group
  relations
    define member: [user]
type doc
  relations
    define viewer: [group#member]
`,
			typ: "group", rel: "member",
			want: []string{"doc", "group"},
		},
		{
			name: "wildcard grants add no edge",
			dsl: `model
  schema 1.1
type user
type doc
  relations
    define viewer: [user:*]
`,
			typ: "doc", rel: "viewer",
			want: []string{"doc"},
		},
		{
			name: "intersection depends on both operands",
			dsl: `model
  schema 1.1
type user
type doc
  relations
    define member: [user]
    define active: [user]
    define viewer: member and active
`,
			typ: "doc", rel: "active",
			want: []string{"doc"},
		},
		{
			name: "exclusion depends on the subtracted operand too",
			dsl: `model
  schema 1.1
type user
type team
  relations
    define blocked: [user]
type doc
  relations
    define team: [team]
    define member: [user]
    define viewer: member but not blocked from team
`,
			typ: "team", rel: "blocked",
			want: []string{"doc", "team"},
		},
		{
			name: "mutual recursion terminates and covers the cycle",
			dsl: `model
  schema 1.1
type user
type a
  relations
    define link: [b]
    define viewer: [user] or viewer from link
type b
  relations
    define link: [a]
    define viewer: [user] or viewer from link
`,
			typ: "a", rel: "viewer",
			want: []string{"a", "b"},
		},
		{
			name: "three hop chain reaches the far end",
			dsl: `model
  schema 1.1
type user
type one
  relations
    define viewer: [user]
type two
  relations
    define up: [one]
    define viewer: viewer from up
type three
  relations
    define up: [two]
    define viewer: viewer from up
`,
			typ: "one", rel: "viewer",
			want: []string{"one", "three", "two"},
		},
		{
			name: "unrelated type is not in the closure",
			dsl: `model
  schema 1.1
type user
type doc
  relations
    define viewer: [user]
type note
  relations
    define viewer: [user]
`,
			typ: "doc", rel: "viewer",
			want: []string{"doc"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := graphFromDSL(t, tt.dsl)
			got, ok := g.affectedTypes(tt.typ, tt.rel)
			require.True(t, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestNewModelGraphRejectsUnusableModels(t *testing.T) {
	str := func(s string) *string { return &s }
	emptyMap := map[string]interface{}{}

	tests := []struct {
		name    string
		model   *openfga.AuthorizationModel
		wantErr string
	}{
		{name: "nil model", model: nil, wantErr: "nil"},
		{
			name:    "schema version 1.0",
			model:   &openfga.AuthorizationModel{SchemaVersion: "1.0"},
			wantErr: "schema version",
		},
		{
			name: "userset with no rewrite",
			model: &openfga.AuthorizationModel{
				SchemaVersion: modelSchemaVersion,
				TypeDefinitions: []openfga.TypeDefinition{{
					Type:      "doc",
					Relations: &map[string]openfga.Userset{"viewer": {}},
				}},
			},
			wantErr: "no recognized rewrite",
		},
		{
			name: "computed userset referencing an undefined relation",
			model: &openfga.AuthorizationModel{
				SchemaVersion: modelSchemaVersion,
				TypeDefinitions: []openfga.TypeDefinition{{
					Type: "doc",
					Relations: &map[string]openfga.Userset{
						"viewer": {ComputedUserset: &openfga.ObjectRelation{Relation: str("missing")}},
					},
				}},
			},
			wantErr: "undefined relation doc#missing",
		},
		{
			name: "computed userset with no relation",
			model: &openfga.AuthorizationModel{
				SchemaVersion: modelSchemaVersion,
				TypeDefinitions: []openfga.TypeDefinition{{
					Type:      "doc",
					Relations: &map[string]openfga.Userset{"viewer": {ComputedUserset: &openfga.ObjectRelation{}}},
				}},
			},
			wantErr: "no relation",
		},
		{
			name: "tuple to userset missing a relation",
			model: &openfga.AuthorizationModel{
				SchemaVersion: modelSchemaVersion,
				TypeDefinitions: []openfga.TypeDefinition{{
					Type: "doc",
					Relations: &map[string]openfga.Userset{
						"viewer": {TupleToUserset: &openfga.TupleToUserset{}},
					},
				}},
			},
			wantErr: "missing a relation",
		},
		{
			name: "tupleset referencing an undefined relation",
			model: &openfga.AuthorizationModel{
				SchemaVersion: modelSchemaVersion,
				TypeDefinitions: []openfga.TypeDefinition{{
					Type: "doc",
					Relations: &map[string]openfga.Userset{
						"viewer": {TupleToUserset: &openfga.TupleToUserset{
							Tupleset:        openfga.ObjectRelation{Relation: str("parent")},
							ComputedUserset: openfga.ObjectRelation{Relation: str("viewer")},
						}},
					},
				}},
			},
			wantErr: "undefined relation doc#parent",
		},
		{
			name: "direct userset referencing an undefined relation",
			model: &openfga.AuthorizationModel{
				SchemaVersion: modelSchemaVersion,
				TypeDefinitions: []openfga.TypeDefinition{{
					Type: "doc",
					Relations: &map[string]openfga.Userset{
						"viewer": {This: &emptyMap},
					},
					Metadata: &openfga.Metadata{Relations: &map[string]openfga.RelationMetadata{
						"viewer": {DirectlyRelatedUserTypes: &[]openfga.RelationReference{
							{Type: "group", Relation: str("member")},
						}},
					}},
				}},
			},
			wantErr: "undefined relation group#member",
		},
		{
			name: "unsupported rewrite nested inside a union",
			model: &openfga.AuthorizationModel{
				SchemaVersion: modelSchemaVersion,
				TypeDefinitions: []openfga.TypeDefinition{{
					Type: "doc",
					Relations: &map[string]openfga.Userset{
						"viewer": {Union: &openfga.Usersets{Child: []openfga.Userset{{}}}},
					},
				}},
			},
			wantErr: "no recognized rewrite",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g, err := newModelGraph(tt.model)
			require.Error(t, err)
			assert.Nil(t, g)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestNewModelGraphIgnoresTypesWithoutRelations(t *testing.T) {
	g := graphFromDSL(t, "model\n  schema 1.1\ntype user\ntype doc\n  relations\n    define viewer: [user]\n")
	_, ok := g.affectedTypes("user", "anything")
	assert.False(t, ok)
	got, ok := g.affectedTypes("doc", "viewer")
	require.True(t, ok)
	assert.Equal(t, []string{"doc"}, got)
}

func TestLoadModelGraph(t *testing.T) {
	t.Run("builds the graph from the model OpenFGA returns", func(t *testing.T) {
		client := new(MockFgaClient)
		client.On("ReadAuthorizationModel", context.Background()).
			Return(parseModelDSL(t, platformModelDSL(t)), nil)

		g, err := loadModelGraph(context.Background(), client)
		require.NoError(t, err)
		got, ok := g.affectedTypes("meeting", "participant")
		require.True(t, ok)
		assert.Equal(t, []string{"meeting", "meeting_attachment"}, got)
		client.AssertExpectations(t)
	})

	t.Run("a read error is returned wrapped", func(t *testing.T) {
		client := new(MockFgaClient)
		boom := errors.New("openfga down")
		client.On("ReadAuthorizationModel", context.Background()).
			Return((*openfga.AuthorizationModel)(nil), boom)

		g, err := loadModelGraph(context.Background(), client)
		require.ErrorIs(t, err, boom)
		assert.Nil(t, g)
	})

	t.Run("an uninterpretable model is an error", func(t *testing.T) {
		client := new(MockFgaClient)
		client.On("ReadAuthorizationModel", context.Background()).
			Return(&openfga.AuthorizationModel{SchemaVersion: "1.0"}, nil)

		g, err := loadModelGraph(context.Background(), client)
		require.Error(t, err)
		assert.Nil(t, g)
	})
}

// ── Soundness against a reference evaluator ──────────────────────────────────

// refTuple is one stored relationship, in OpenFGA's subject notation:
// "user:a", "user:*", "team:t1#member" or "project:1".
type refTuple struct {
	object   string
	relation string
	subject  string
}

// refEvaluator is a small, independent implementation of OpenFGA check
// semantics for models made of this, computed, tuple-to-userset, union,
// intersection and exclusion rewrites. It exists only to test the
// invalidation graph: the graph is sound if no check result ever changes
// outside the scope the graph predicts for the tuple that changed.
type refEvaluator struct {
	model  map[string]map[string]openfga.Userset
	tuples map[refTuple]struct{}
}

func newRefEvaluator(model *openfga.AuthorizationModel) *refEvaluator {
	defs := make(map[string]map[string]openfga.Userset)
	for _, td := range model.TypeDefinitions {
		if td.Relations != nil {
			defs[td.Type] = *td.Relations
		}
	}
	return &refEvaluator{model: defs, tuples: make(map[refTuple]struct{})}
}

func splitObjectRef(object string) (typ, id string) {
	typ, id, _ = strings.Cut(object, ":")
	return typ, id
}

// check reports whether user has relation on object. visiting breaks cycles,
// which gives the least fixpoint OpenFGA also computes for these models.
func (e *refEvaluator) check(object, relation, user string, visiting map[string]bool) bool {
	key := object + "#" + relation + "@" + user
	if visiting[key] {
		return false
	}
	visiting[key] = true
	defer delete(visiting, key)

	typ, _ := splitObjectRef(object)
	userset, ok := e.model[typ][relation]
	if !ok {
		return false
	}
	return e.eval(userset, object, relation, user, visiting)
}

func (e *refEvaluator) eval(u openfga.Userset, object, relation, user string, visiting map[string]bool) bool {
	switch {
	case u.This != nil:
		for tuple := range e.tuples {
			if tuple.object != object || tuple.relation != relation {
				continue
			}
			switch {
			case tuple.subject == user, tuple.subject == "user:*":
				return true
			case strings.Contains(tuple.subject, "#"):
				subjectObject, subjectRelation, _ := strings.Cut(tuple.subject, "#")
				if e.check(subjectObject, subjectRelation, user, visiting) {
					return true
				}
			}
		}
		return false
	case u.ComputedUserset != nil:
		return e.check(object, *u.ComputedUserset.Relation, user, visiting)
	case u.TupleToUserset != nil:
		tupleset := *u.TupleToUserset.Tupleset.Relation
		computed := *u.TupleToUserset.ComputedUserset.Relation
		for tuple := range e.tuples {
			if tuple.object != object || tuple.relation != tupleset || strings.Contains(tuple.subject, "#") {
				continue
			}
			if e.check(tuple.subject, computed, user, visiting) {
				return true
			}
		}
		return false
	case u.Union != nil:
		for _, child := range u.Union.Child {
			if e.eval(child, object, relation, user, visiting) {
				return true
			}
		}
		return false
	case u.Intersection != nil:
		for _, child := range u.Intersection.Child {
			if !e.eval(child, object, relation, user, visiting) {
				return false
			}
		}
		return true
	case u.Difference != nil:
		return e.eval(u.Difference.Base, object, relation, user, visiting) &&
			!e.eval(u.Difference.Subtract, object, relation, user, visiting)
	}
	return false
}

// refUniverse enumerates every check in a small closed world: each defined
// relation on each of a few object ids, for each of a few users.
type refUniverse struct {
	objectIDs []string
	users     []string
}

func (u refUniverse) snapshot(e *refEvaluator) map[string]bool {
	out := make(map[string]bool)
	for typ, relations := range e.model {
		for relation := range relations {
			for _, id := range u.objectIDs {
				object := typ + ":" + id
				for _, user := range u.users {
					out[object+"#"+relation+"@"+user] = e.check(object, relation, user, map[string]bool{})
				}
			}
		}
	}
	return out
}

// candidateTuples lists every tuple the model's type restrictions allow within
// the closed world.
func (u refUniverse) candidateTuples(model *openfga.AuthorizationModel) []refTuple {
	var out []refTuple
	for _, td := range model.TypeDefinitions {
		if td.Metadata == nil || td.Metadata.Relations == nil {
			continue
		}
		for relation, meta := range *td.Metadata.Relations {
			if meta.DirectlyRelatedUserTypes == nil {
				continue
			}
			for _, id := range u.objectIDs {
				object := td.Type + ":" + id
				for _, ref := range *meta.DirectlyRelatedUserTypes {
					switch {
					case ref.Wildcard != nil:
						out = append(out, refTuple{object, relation, ref.Type + ":*"})
					case ref.Relation != nil:
						for _, sid := range u.objectIDs {
							out = append(out, refTuple{object, relation, fmt.Sprintf("%s:%s#%s", ref.Type, sid, *ref.Relation)})
						}
					case ref.Type == "user":
						for _, user := range u.users {
							out = append(out, refTuple{object, relation, user})
						}
					default:
						for _, sid := range u.objectIDs {
							out = append(out, refTuple{object, relation, ref.Type + ":" + sid})
						}
					}
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		return a.object+a.relation+a.subject < b.object+b.relation+b.subject
	})
	return out
}

// assertPlanCoversChanges is the soundness property: every check whose result
// differs between before and after must be covered by the plan, either
// because the plan is global, because its object type has a type marker, or
// because its (type, user) pair has a user marker.
func assertPlanCoversChanges(
	t *testing.T,
	plan invalidationPlan,
	before, after map[string]bool,
	change string,
) (changed int) {
	t.Helper()
	typeMarkers := make(map[string]bool)
	for _, typ := range plan.types {
		typeMarkers[typ] = true
	}
	userMarkers := make(map[userMarker]bool)
	for _, m := range plan.users {
		userMarkers[m] = true
	}

	for key, was := range before {
		if after[key] == was {
			continue
		}
		changed++
		if plan.global {
			continue
		}
		object, rest, _ := strings.Cut(key, "#")
		_, user, _ := strings.Cut(rest, "@")
		typ, _ := splitObjectRef(object)
		covered := typeMarkers[typ] || userMarkers[userMarker{typ: typ, user: user}]
		assert.True(t, covered,
			"%s changed %q (%v -> %v) but plan %+v does not cover it", change, key, was, after[key], plan)
	}
	return changed
}

// TestPlanInvalidationIsSoundOnPlatformModel mutates a random but valid tuple
// set one tuple at a time and checks, with an independent evaluator, that
// every check result that flips is covered by the scoped plan. This is the
// property the whole optimization rests on: scoping is only allowed to leave
// a cache entry alone when its result provably cannot have changed.
func TestPlanInvalidationIsSoundOnPlatformModel(t *testing.T) {
	model := parseModelDSL(t, platformModelDSL(t))
	graph, err := newModelGraph(model)
	require.NoError(t, err)

	universe := refUniverse{
		objectIDs: []string{"1", "2"},
		users:     []string{"user:a", "user:b"},
	}
	candidates := universe.candidateTuples(model)
	require.NotEmpty(t, candidates)

	const (
		trials            = 12
		mutationsPerTrial = 25
		seedTuples        = 70
	)

	var scopedPlans, mutationsThatChangedSomething int
	for trial := 0; trial < trials; trial++ {
		rng := rand.New(rand.NewSource(int64(trial) + 1)) //nolint:gosec // deterministic test data
		e := newRefEvaluator(model)
		for i := 0; i < seedTuples; i++ {
			e.tuples[candidates[rng.Intn(len(candidates))]] = struct{}{}
		}

		for m := 0; m < mutationsPerTrial; m++ {
			tuple := candidates[rng.Intn(len(candidates))]
			_, present := e.tuples[tuple]
			before := universe.snapshot(e)

			var ref tupleRef
			ref = tupleRef{user: tuple.subject, relation: tuple.relation, object: tuple.object}
			plan := graph.planInvalidation([]tupleRef{ref})

			if present {
				delete(e.tuples, tuple)
			} else {
				e.tuples[tuple] = struct{}{}
			}
			after := universe.snapshot(e)

			op := "write"
			if present {
				op = "delete"
			}
			change := fmt.Sprintf("trial %d %s %s#%s@%s", trial, op, tuple.object, tuple.relation, tuple.subject)
			if assertPlanCoversChanges(t, plan, before, after, change) > 0 {
				mutationsThatChangedSomething++
			}
			if !plan.global {
				scopedPlans++
			}
		}
	}

	// Guard against a vacuous pass: the run must have exercised scoped plans
	// and real result flips.
	assert.Positive(t, scopedPlans)
	assert.Positive(t, mutationsThatChangedSomething)
}

// TestPlanInvalidationIsSoundOnSyntheticModels runs the same property on
// models using intersection and exclusion, whose results can flip in either
// direction when a tuple is added.
func TestPlanInvalidationIsSoundOnSyntheticModels(t *testing.T) {
	models := map[string]string{
		"exclusion": `model
  schema 1.1
type user
type team
  relations
    define member: [user]
    define blocked: [user, team#member]
type doc
  relations
    define team: [team]
    define editor: [user, team#member]
    define viewer: editor but not blocked from team
`,
		"intersection": `model
  schema 1.1
type user
type org
  relations
    define active: [user]
type doc
  relations
    define org: [org]
    define member: [user]
    define viewer: member and active from org
`,
		// A tuple on a tupleset relation names the object a rewrite traverses,
		// so a concrete-user subject there changes other users' checks. These
		// shapes are not in the platform model today but the graph is built
		// from whatever model is live.
		"user as tupleset subject": `model
  schema 1.1
type user
  relations
    define friend: [user]
type doc
  relations
    define owner_user: [user]
    define viewer: friend from owner_user
`,
		"user as inheritance parent": `model
  schema 1.1
type user
  relations
    define viewer: [user]
type doc
  relations
    define parent: [user]
    define viewer: viewer from parent
`,
		"recursive parents": `model
  schema 1.1
type user
type folder
  relations
    define parent: [folder]
    define viewer: [user, user:*] or viewer from parent
type doc
  relations
    define folder: [folder]
    define viewer: viewer from folder
`,
	}

	// Object ids overlap with user ids so that a user can also be the object
	// a tupleset points at.
	universe := refUniverse{objectIDs: []string{"a", "b", "c"}, users: []string{"user:a", "user:b", "user:c"}}

	for name, dsl := range models {
		t.Run(name, func(t *testing.T) {
			model := parseModelDSL(t, dsl)
			graph, err := newModelGraph(model)
			require.NoError(t, err)
			candidates := universe.candidateTuples(model)
			require.NotEmpty(t, candidates)

			var flips int
			for trial := 0; trial < 20; trial++ {
				rng := rand.New(rand.NewSource(int64(trial) + 100)) //nolint:gosec // deterministic test data
				e := newRefEvaluator(model)
				for i := 0; i < 8; i++ {
					e.tuples[candidates[rng.Intn(len(candidates))]] = struct{}{}
				}
				for m := 0; m < 20; m++ {
					tuple := candidates[rng.Intn(len(candidates))]
					_, present := e.tuples[tuple]
					before := universe.snapshot(e)
					plan := graph.planInvalidation([]tupleRef{{user: tuple.subject, relation: tuple.relation, object: tuple.object}})
					if present {
						delete(e.tuples, tuple)
					} else {
						e.tuples[tuple] = struct{}{}
					}
					flips += assertPlanCoversChanges(t, plan, before, universe.snapshot(e),
						fmt.Sprintf("%s trial %d tuple %+v", name, trial, tuple))
				}
			}
			assert.Positive(t, flips, "the random walk must flip at least one check result")
		})
	}
}
