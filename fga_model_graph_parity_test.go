// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package main

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
)

// reachableRelations returns every relation whose result can change when
// typ#rel changes, following the same edges affectedTypes uses.
func reachableRelations(g *modelGraph, typ, rel string) map[relNode]bool {
	start := relNode{typ: typ, rel: rel}
	seen := map[relNode]bool{start: true}
	queue := []relNode{start}
	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]
		for _, next := range g.dependents[node] {
			if !seen[next] {
				seen[next] = true
				queue = append(queue, next)
			}
		}
	}
	return seen
}

// TestModelGraphRelationLevelDependencies pins the relation-level chains that
// cascading and cross-type cache invalidation must not miss. They are the
// scenarios that hand-maintained dependency maps have historically got wrong
// (guard relations that read a cascading relation, grants that feed a
// cascading relation on the same object, parent/child hierarchy edges,
// and the project_membership <-> b2b_org cycle). The graph is derived from
// the model, so each case checks the derivation rather than a copy of it.
func TestModelGraphRelationLevelDependencies(t *testing.T) {
	g := platformGraph(t)

	tests := []struct {
		name    string
		typ     string
		rel     string
		reaches []relNode
		// notReaches lists relations the source must not feed.
		notReaches []relNode
	}{
		{
			name: "project owner reaches every guard and derived relation",
			typ:  "project", rel: "owner",
			reaches: []relNode{
				{"project", "writer"}, {"project", "auditor"},
				{"project", "writer_guard"}, {"project", "auditor_guard"},
				{"project", "viewer"}, {"project", "meetings_creator"},
				{"project", "mentorship_program_creator"},
			},
			notReaches: []relNode{{"project", "campaign_manager"}},
		},
		{
			name: "executive_director feeds auditor, marketing_auditor and campaign_manager",
			typ:  "project", rel: "executive_director",
			reaches: []relNode{
				{"project", "auditor"}, {"project", "marketing_auditor"},
				{"project", "auditor_guard"}, {"project", "viewer"},
				{"project", "campaign_manager"},
			},
		},
		{
			name: "global_marketing_ops feeds marketing_auditor and campaign_manager",
			typ:  "project", rel: "global_marketing_ops",
			reaches: []relNode{
				{"project", "marketing_auditor"}, {"project", "campaign_manager"},
			},
			notReaches: []relNode{{"project", "viewer"}, {"project", "writer"}},
		},
		{
			name: "marketing_ops cascades to marketing_auditor and campaign_manager",
			typ:  "project", rel: "marketing_ops",
			reaches: []relNode{
				{"project", "marketing_ops"}, {"project", "marketing_auditor"},
				{"project", "campaign_manager"},
			},
			notReaches: []relNode{{"project", "viewer"}, {"committee", "viewer"}},
		},
		{
			name: "project parent edge reaches cascading relations",
			typ:  "project", rel: "parent",
			reaches: []relNode{
				{"project", "writer"}, {"project", "auditor"},
				{"project", "marketing_ops"}, {"project", "campaign_manager"},
				{"project", "viewer"},
			},
		},
		{
			name: "project writer_guard reaches committee, meeting and vote relations",
			typ:  "project", rel: "writer_guard",
			reaches: []relNode{
				{"committee", "writer"}, {"meeting", "organizer"},
			},
		},
		{
			name: "b2b_org child edge reaches writer and auditor",
			typ:  "b2b_org", rel: "child",
			reaches: []relNode{{"b2b_org", "writer"}, {"b2b_org", "auditor"}},
		},
		{
			name: "b2b_org parent edge reaches writer and auditor",
			typ:  "b2b_org", rel: "parent",
			reaches: []relNode{{"b2b_org", "writer"}, {"b2b_org", "auditor"}},
		},
		{
			name: "project_membership key_contact reaches b2b_org auditor",
			typ:  "project_membership", rel: "key_contact",
			reaches: []relNode{{"b2b_org", "auditor"}, {"project_membership", "auditor"}},
		},
		{
			name: "b2b_org auditor reaches project_membership auditor (cycle back)",
			typ:  "b2b_org", rel: "auditor",
			reaches: []relNode{{"project_membership", "auditor"}, {"b2b_org", "auditor"}},
		},
		{
			name: "team member reaches direct userset consumers",
			typ:  "team", rel: "member",
			reaches: []relNode{
				{"project", "marketing_ops"}, {"project", "auditor"},
				{"committee", "auditor"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := reachableRelations(g, tt.typ, tt.rel)
			for _, want := range tt.reaches {
				assert.True(t, got[want], "%s#%s must reach %s#%s", tt.typ, tt.rel, want.typ, want.rel)
			}
			for _, no := range tt.notReaches {
				assert.False(t, got[no], "%s#%s must not reach %s#%s", tt.typ, tt.rel, no.typ, no.rel)
			}
		})
	}
}

// TestModelGraphIrrelevantRelationsStayInOwnType covers relations that no
// other type reads: writing them must not invalidate any other type.
func TestModelGraphIrrelevantRelationsStayInOwnType(t *testing.T) {
	g := platformGraph(t)

	for _, rel := range []string{"marketing_ops", "global_marketing_ops"} {
		t.Run("project#"+rel, func(t *testing.T) {
			types, ok := g.affectedTypes("project", rel)
			assert.True(t, ok)
			assert.Equal(t, []string{"project"}, types)
		})
	}

	t.Run("cycle types are kept and sorted", func(t *testing.T) {
		types, ok := g.affectedTypes("project_membership", "key_contact")
		assert.True(t, ok)
		assert.True(t, sort.StringsAreSorted(types))
		assert.Contains(t, types, "b2b_org")
		assert.Contains(t, types, "project_membership")
	})
}
