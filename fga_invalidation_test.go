// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	openfga "github.com/openfga/go-sdk"
	. "github.com/openfga/go-sdk/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

var invalidationTestBase = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

func b32(s string) string { return cacheKeyEncoder.EncodeToString([]byte(s)) }

func TestParseInvalidationMode(t *testing.T) {
	tests := []struct {
		in     string
		want   invalidationMode
		wantOK bool
		str    string
	}{
		{in: "", want: invalidationGlobal, wantOK: true, str: modeNameGlobal},
		{in: modeNameGlobal, want: invalidationGlobal, wantOK: true, str: modeNameGlobal},
		{in: modeNameReadScoped, want: invalidationReadScoped, wantOK: true, str: modeNameReadScoped},
		{in: modeNameScoped, want: invalidationScoped, wantOK: true, str: modeNameScoped},
		{in: "  Scoped\n", want: invalidationScoped, wantOK: true, str: modeNameScoped},
		{in: "READ-SCOPED", want: invalidationReadScoped, wantOK: true, str: modeNameReadScoped},
		{in: "true", want: invalidationGlobal, wantOK: false, str: modeNameGlobal},
		{in: "scoped-writes", want: invalidationGlobal, wantOK: false, str: modeNameGlobal},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%q", tt.in), func(t *testing.T) {
			got, ok := parseInvalidationMode(tt.in)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.str, got.String())
		})
	}
}

func TestInvalidationConfigCapabilities(t *testing.T) {
	g := graphFromDSL(t, "model\n  schema 1.1\ntype user\ntype doc\n  relations\n    define viewer: [user]\n")
	tests := []struct {
		name       string
		cfg        invalidationConfig
		readScoped bool
		writeScope bool
	}{
		{name: modeNameGlobal, cfg: invalidationConfig{mode: invalidationGlobal, graph: g}},
		{name: "read-scoped never writes scoped", cfg: invalidationConfig{mode: invalidationReadScoped, graph: g}, readScoped: true},
		{name: "scoped with graph", cfg: invalidationConfig{mode: invalidationScoped, graph: g}, readScoped: true, writeScope: true},
		{name: "scoped without graph reads only", cfg: invalidationConfig{mode: invalidationScoped}, readScoped: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.readScoped, tt.cfg.readsScoped())
			assert.Equal(t, tt.writeScope, tt.cfg.writesScoped())
		})
	}
}

func TestObjectType(t *testing.T) {
	tests := []struct {
		object string
		want   string
		ok     bool
	}{
		{"meeting:abc", "meeting", true},
		{"v1_past_meeting:123", "v1_past_meeting", true},
		{"my-type:1", "my-type", true},
		{"Meeting:ABC", "Meeting", true},
		{"project:a:b", "project", true},
		{"", "", false},
		{"nocolon", "", false},
		{":id", "", false},
		{"type:", "", false},
		{"bad type:1", "", false},
		{"a.b:1", "", false},
		{"a#b:1", "", false},
		{"a*:1", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.object, func(t *testing.T) {
			got, ok := objectType(tt.object)
			assert.Equal(t, tt.ok, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestIsConcreteUser(t *testing.T) {
	tests := []struct {
		subject string
		want    bool
	}{
		{"user:alice", true},
		{"user:auth0|123", true},
		{"user:*", false},
		{"user:", false},
		{"user", false},
		{"", false},
		{"team:t1#member", false},
		{"user:alice#member", false},
		{"project:p1", false},
		{"committee:c1#member", false},
	}
	for _, tt := range tests {
		t.Run(tt.subject, func(t *testing.T) {
			assert.Equal(t, tt.want, isConcreteUser(tt.subject))
		})
	}
}

func TestMarkerKeys(t *testing.T) {
	assert.Equal(t, "inv.t.meeting", typeMarkerKey("meeting"))
	assert.Equal(t, "inv.u.meeting."+b32("user:a"), userMarkerKey("meeting", "user:a"))

	t.Run("keys are distinct across kinds, types and users", func(t *testing.T) {
		keys := []string{
			globalInvalidationKey,
			typeMarkerKey("meeting"), typeMarkerKey("vote"),
			userMarkerKey("meeting", "user:a"), userMarkerKey("meeting", "user:b"),
			userMarkerKey("vote", "user:a"),
		}
		seen := make(map[string]bool)
		for _, k := range keys {
			assert.False(t, seen[k], "duplicate key %q", k)
			seen[k] = true
		}
	})

	t.Run("user marker keys are valid NATS KV keys", func(t *testing.T) {
		key := userMarkerKey("v1_past_meeting", "user:auth0|a/b c@d.e")
		assert.NotContains(t, key, "|")
		assert.NotContains(t, key, "/")
		assert.NotContains(t, key, " ")
		assert.NotContains(t, key, "@")
		assert.NotContains(t, key, "=")
	})

	t.Run("a user marker cannot collide with a type marker", func(t *testing.T) {
		assert.False(t, strings.HasPrefix(userMarkerKey("meeting", "user:a"), typeMarkerPrefix))
		assert.False(t, strings.HasPrefix(typeMarkerKey("meeting"), userMarkerPrefix))
	})

	t.Run("markerKeys lists type markers then user markers", func(t *testing.T) {
		plan := invalidationPlan{
			types: []string{"b", "a"},
			users: []userMarker{{typ: "c", user: "user:x"}},
		}
		assert.Equal(t, []string{"inv.t.b", "inv.t.a", "inv.u.c." + b32("user:x")}, plan.markerKeys())
	})
}

func TestTupleRefs(t *testing.T) {
	cond := &openfga.RelationshipCondition{Name: "c"}
	refs := tupleRefs(
		[]ClientTupleKey{
			{User: "user:a", Relation: "writer", Object: "project:p"},
			{User: "user:b", Relation: "viewer", Object: "project:p", Condition: cond},
		},
		[]ClientTupleKeyWithoutCondition{{User: "user:c", Relation: "owner", Object: "project:q"}},
	)
	assert.Equal(t, []tupleRef{
		{user: "user:a", relation: "writer", object: "project:p"},
		{user: "user:b", relation: "viewer", object: "project:p", conditioned: true},
		{user: "user:c", relation: "owner", object: "project:q"},
	}, refs)
	assert.Empty(t, tupleRefs(nil, nil))
}

// ── Plan building ────────────────────────────────────────────────────────────

func TestPlanInvalidation(t *testing.T) {
	g := platformGraph(t)

	manyUsers := func(n int) []tupleRef {
		refs := make([]tupleRef, 0, n)
		for i := 0; i < n; i++ {
			refs = append(refs, tupleRef{
				user: fmt.Sprintf("user:u%d", i), relation: "participant", object: "meeting:m1",
			})
		}
		return refs
	}

	tests := []struct {
		name   string
		graph  *modelGraph
		tuples []tupleRef
		// want is compared exactly unless assertPlan is set, which is used for
		// closures too large to list.
		want       invalidationPlan
		assertPlan func(t *testing.T, got invalidationPlan)
	}{
		{
			name:  "user subject scopes to that user on the affected types",
			graph: g,
			tuples: []tupleRef{
				{user: "user:a", relation: "participant", object: "meeting:m1"},
			},
			want: invalidationPlan{users: []userMarker{
				{typ: "meeting", user: "user:a"},
				{typ: "meeting_attachment", user: "user:a"},
			}},
		},
		{
			name:  "a delete of the same tuple plans identically",
			graph: g,
			tuples: []tupleRef{
				{user: "user:a", relation: "participant", object: "meeting:m9"},
			},
			want: invalidationPlan{users: []userMarker{
				{typ: "meeting", user: "user:a"},
				{typ: "meeting_attachment", user: "user:a"},
			}},
		},
		{
			name:  "vote participant never touches meetings",
			graph: g,
			tuples: []tupleRef{
				{user: "user:a", relation: "participant", object: "vote:v1"},
			},
			want: invalidationPlan{users: []userMarker{{typ: "vote", user: "user:a"}}},
		},
		{
			name:  "wildcard subject scopes to types, not users",
			graph: g,
			tuples: []tupleRef{
				{user: "user:*", relation: "viewer", object: "meeting:m1"},
			},
			want: invalidationPlan{types: []string{"meeting"}},
		},
		{
			name:  "object subject (parent link) scopes to types",
			graph: g,
			tuples: []tupleRef{
				{user: "project:p1", relation: "parent", object: "project:p2"},
			},
			assertPlan: func(t *testing.T, got invalidationPlan) {
				assert.False(t, got.global)
				assert.Empty(t, got.users)
				assert.Contains(t, got.types, "project")
				assert.Contains(t, got.types, "meeting")
				assert.Contains(t, got.types, "mentorship_task")
				assert.NotContains(t, got.types, "team")
				assert.NotContains(t, got.types, "b2b_org")
			},
		},
		{
			name:  "userset subject scopes to types",
			graph: g,
			tuples: []tupleRef{
				{user: "team:t1#member", relation: "auditor", object: "project:p1"},
			},
			assertPlan: func(t *testing.T, got invalidationPlan) {
				assert.False(t, got.global)
				assert.Empty(t, got.users)
				assert.Contains(t, got.types, "project")
				assert.Contains(t, got.types, "committee")
				assert.Contains(t, got.types, "vote")
				assert.NotContains(t, got.types, "b2b_org")
				assert.NotContains(t, got.types, "team")
			},
		},
		{
			name:  "user added to a team reaches every type granting to team members",
			graph: g,
			tuples: []tupleRef{
				{user: "user:a", relation: "member", object: "team:t1"},
			},
			assertPlan: assertTeamMemberPlan,
		},
		{
			name:  "committee writer removal reaches inheriting types for that user only",
			graph: g,
			tuples: []tupleRef{
				{user: "user:a", relation: "writer", object: "committee:c1"},
			},
			want: invalidationPlan{users: usersFor("user:a",
				"committee", "committee_application", "committee_invite", "groupsio_mailing_list",
				"meeting", "meeting_attachment", "past_meeting", "past_meeting_attachment",
				"survey", "survey_response", "v1_meeting", "v1_past_meeting", "vote", "vote_response")},
		},
		{
			name:  "a type marker from one tuple covers user markers for that type from another",
			graph: g,
			tuples: []tupleRef{
				{user: "user:a", relation: "participant", object: "meeting:m1"},
				{user: "user:*", relation: "viewer", object: "meeting:m1"},
			},
			want: invalidationPlan{
				types: []string{"meeting"},
				users: []userMarker{{typ: "meeting_attachment", user: "user:a"}},
			},
		},
		{
			name:  "the same user and relation on many objects collapses to one marker set",
			graph: g,
			tuples: []tupleRef{
				{user: "user:a", relation: "participant", object: "meeting:m1"},
				{user: "user:a", relation: "participant", object: "meeting:m2"},
				{user: "user:a", relation: "participant", object: "meeting:m3"},
			},
			want: invalidationPlan{users: []userMarker{
				{typ: "meeting", user: "user:a"},
				{typ: "meeting_attachment", user: "user:a"},
			}},
		},
		{
			name:  "different users get their own markers",
			graph: g,
			tuples: []tupleRef{
				{user: "user:b", relation: "participant", object: "vote:v1"},
				{user: "user:a", relation: "participant", object: "vote:v1"},
			},
			want: invalidationPlan{users: []userMarker{
				{typ: "vote", user: "user:a"},
				{typ: "vote", user: "user:b"},
			}},
		},
		{
			name:  "user markers right at the cap stay scoped",
			graph: g,
			// 16 users x 2 types = 32 markers.
			tuples: manyUsers(16),
			want: invalidationPlan{users: func() []userMarker {
				var out []userMarker
				for _, typ := range []string{"meeting", "meeting_attachment"} {
					for i := 0; i < 16; i++ {
						out = append(out, userMarker{typ: typ, user: fmt.Sprintf("user:u%d", i)})
					}
				}
				return sortedUserMarkers(out)
			}()},
		},
		{
			name:   "user markers over the cap fold into type markers",
			graph:  g,
			tuples: manyUsers(17), // 34 markers
			want:   invalidationPlan{types: []string{"meeting", "meeting_attachment"}},
		},
		{
			name:   "a large batch of one user across many types folds only if over the cap",
			graph:  g,
			tuples: manyUsers(100),
			want:   invalidationPlan{types: []string{"meeting", "meeting_attachment"}},
		},
		{name: "no graph is global", graph: nil, tuples: manyUsers(1), want: globalPlan()},
		{name: "no tuples is global", graph: g, tuples: nil, want: globalPlan()},
		{
			name:   "unknown object type is global",
			graph:  g,
			tuples: []tupleRef{{user: "user:a", relation: "viewer", object: "widget:w1"}},
			want:   globalPlan(),
		},
		{
			name:   "unknown relation is global",
			graph:  g,
			tuples: []tupleRef{{user: "user:a", relation: "no_such_relation", object: "meeting:m1"}},
			want:   globalPlan(),
		},
		{
			name:   "malformed object is global",
			graph:  g,
			tuples: []tupleRef{{user: "user:a", relation: "viewer", object: "meeting"}},
			want:   globalPlan(),
		},
		{
			name:   "unsafe object type is global",
			graph:  g,
			tuples: []tupleRef{{user: "user:a", relation: "viewer", object: "mee.ting:1"}},
			want:   globalPlan(),
		},
		{
			name:   "conditioned tuple is global",
			graph:  g,
			tuples: []tupleRef{{user: "user:a", relation: "participant", object: "meeting:m1", conditioned: true}},
			want:   globalPlan(),
		},
		{
			name:  "one bad tuple makes the whole batch global",
			graph: g,
			tuples: []tupleRef{
				{user: "user:a", relation: "participant", object: "meeting:m1"},
				{user: "user:a", relation: "viewer", object: "widget:w1"},
			},
			want: globalPlan(),
		},
		{
			name:  "one conditioned tuple makes the whole batch global",
			graph: g,
			tuples: []tupleRef{
				{user: "user:a", relation: "participant", object: "meeting:m1"},
				{user: "user:b", relation: "participant", object: "meeting:m1", conditioned: true},
			},
			want: globalPlan(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.graph.planInvalidation(tt.tuples)
			if tt.assertPlan != nil {
				tt.assertPlan(t, got)
				return
			}
			assert.Equal(t, tt.want.global, got.global)
			assert.Equal(t, tt.want.types, got.types)
			assert.Equal(t, tt.want.users, got.users)
		})
	}
}

// assertTeamMemberPlan checks the plan for user:a joining team:t1. The closure
// is large, so assert its shape rather than listing every type.
func assertTeamMemberPlan(t *testing.T, plan invalidationPlan) {
	t.Helper()
	require.False(t, plan.global)
	assert.Empty(t, plan.types, "a user subject must not produce type markers")
	byType := make(map[string]bool)
	for _, m := range plan.users {
		assert.Equal(t, "user:a", m.user)
		byType[m.typ] = true
	}
	for _, typ := range []string{"team", "project", "committee", "meeting", "vote", "survey", "b2b_org", "project_application"} {
		assert.True(t, byType[typ], "expected a marker for %s", typ)
	}
	for _, typ := range []string{"mentorship_approver_team", "user"} {
		assert.False(t, byType[typ], "did not expect a marker for %s", typ)
	}
}

func usersFor(user string, types ...string) []userMarker {
	out := make([]userMarker, 0, len(types))
	for _, typ := range types {
		out = append(out, userMarker{typ: typ, user: user})
	}
	return sortedUserMarkers(out)
}

func sortedUserMarkers(in []userMarker) []userMarker {
	out := append([]userMarker(nil), in...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0; j-- {
			a, b := out[j-1], out[j]
			if a.typ > b.typ || (a.typ == b.typ && a.user > b.user) {
				out[j-1], out[j] = out[j], out[j-1]
			}
		}
	}
	return out
}

// ── Marker writes ────────────────────────────────────────────────────────────

// flakyKV wraps MockKeyValue so a test can fail chosen Gets and Puts and
// count Gets.
type flakyKV struct {
	*MockKeyValue
	failGet func(key string) error
	failPut func(key string) error
	gets    atomic.Int64
	getKeys sync.Map
}

func (f *flakyKV) Get(ctx context.Context, key string) (jetstream.KeyValueEntry, error) {
	f.gets.Add(1)
	f.getKeys.Store(key, true)
	if f.failGet != nil {
		if err := f.failGet(key); err != nil {
			return nil, err
		}
	}
	return f.MockKeyValue.Get(ctx, key)
}

func (f *flakyKV) Put(ctx context.Context, key string, value []byte) (uint64, error) {
	if f.failPut != nil {
		if err := f.failPut(key); err != nil {
			return 0, err
		}
	}
	return f.MockKeyValue.Put(ctx, key, value)
}

func (f *flakyKV) PutString(ctx context.Context, key, value string) (uint64, error) {
	return f.Put(ctx, key, []byte(value))
}

func (f *flakyKV) written(key string) bool {
	f.MockKeyValue.mu.Lock()
	defer f.MockKeyValue.mu.Unlock()
	_, ok := f.MockKeyValue.data[key]
	return ok
}

func (f *flakyKV) writtenKeys() []string {
	f.MockKeyValue.mu.Lock()
	defer f.MockKeyValue.mu.Unlock()
	keys := make([]string, 0, len(f.MockKeyValue.data))
	for k := range f.MockKeyValue.data {
		keys = append(keys, k)
	}
	return sortedStrings(keys)
}

func sortedStrings(in []string) []string {
	out := append([]string(nil), in...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

func newFlakyKV() *flakyKV { return &flakyKV{MockKeyValue: NewMockKeyValue()} }

func TestApplyInvalidation(t *testing.T) {
	ctx := context.Background()

	t.Run("a global plan writes only the global key", func(t *testing.T) {
		kv := newFlakyKV()
		before := cacheInvalidationsGlobal.Value()
		require.NoError(t, CacheLayer{bucket: kv}.applyInvalidation(ctx, globalPlan()))
		assert.Equal(t, []string{"inv"}, kv.writtenKeys())
		assert.Equal(t, before+1, cacheInvalidationsGlobal.Value())
	})

	t.Run("a scoped plan writes exactly its markers and not the global key", func(t *testing.T) {
		kv := newFlakyKV()
		plan := invalidationPlan{
			types: []string{"vote"},
			users: []userMarker{{typ: "meeting", user: "user:a"}, {typ: "meeting_attachment", user: "user:a"}},
		}
		beforeScoped, beforeGlobal := cacheInvalidationsScoped.Value(), cacheInvalidationsGlobal.Value()
		require.NoError(t, CacheLayer{bucket: kv}.applyInvalidation(ctx, plan))
		assert.Equal(t, sortedStrings(plan.markerKeys()), kv.writtenKeys())
		assert.False(t, kv.written("inv"))
		assert.Equal(t, beforeScoped+1, cacheInvalidationsScoped.Value())
		assert.Equal(t, beforeGlobal, cacheInvalidationsGlobal.Value())
	})

	t.Run("an empty non-global plan degrades to global rather than doing nothing", func(t *testing.T) {
		kv := newFlakyKV()
		require.NoError(t, CacheLayer{bucket: kv}.applyInvalidation(ctx, invalidationPlan{}))
		assert.Equal(t, []string{"inv"}, kv.writtenKeys())
	})

	t.Run("a failed marker write falls back to the global key", func(t *testing.T) {
		kv := newFlakyKV()
		kv.failPut = func(key string) error {
			if key == typeMarkerKey("vote") {
				return errors.New("kv unavailable")
			}
			return nil
		}
		plan := invalidationPlan{types: []string{"meeting", "vote"}}
		beforeGlobal := cacheInvalidationsGlobal.Value()
		require.NoError(t, CacheLayer{bucket: kv}.applyInvalidation(ctx, plan))
		assert.True(t, kv.written("inv"), "fallback must write the global key")
		assert.Equal(t, beforeGlobal+1, cacheInvalidationsGlobal.Value())
	})

	t.Run("an error is returned when even the global fallback fails", func(t *testing.T) {
		kv := newFlakyKV()
		boom := errors.New("kv down")
		kv.failPut = func(string) error { return boom }
		err := CacheLayer{bucket: kv}.applyInvalidation(ctx, invalidationPlan{types: []string{"vote"}})
		require.ErrorIs(t, err, boom)
	})

	t.Run("a nil bucket is a no-op", func(t *testing.T) {
		require.NoError(t, CacheLayer{}.applyInvalidation(ctx, globalPlan()))
		require.NoError(t, CacheLayer{}.applyInvalidation(ctx, invalidationPlan{types: []string{"vote"}}))
	})

	t.Run("a large scoped plan writes every marker", func(t *testing.T) {
		kv := newFlakyKV()
		plan := invalidationPlan{}
		for i := 0; i < maxScopedUserMarkers; i++ {
			plan.users = append(plan.users, userMarker{typ: "meeting", user: fmt.Sprintf("user:u%d", i)})
		}
		require.NoError(t, CacheLayer{bucket: kv}.applyInvalidation(ctx, plan))
		assert.Len(t, kv.writtenKeys(), maxScopedUserMarkers)
	})
}

// ── Marker reads ─────────────────────────────────────────────────────────────

func TestLoadInvalidationCutoffs(t *testing.T) {
	ctx := context.Background()
	at := func(minutes int) time.Time { return invalidationTestBase.Add(time.Duration(minutes) * time.Minute) }
	item := func(object, user string) ClientBatchCheckItem {
		return ClientBatchCheckItem{Object: object, Relation: "viewer", User: user}
	}

	tests := []struct {
		name    string
		global  time.Time
		markers map[string]time.Time
		failGet map[string]bool
		item    ClientBatchCheckItem
		want    time.Time
	}{
		{name: "no markers falls back to the global time", global: at(5), item: item("meeting:m1", "user:a"), want: at(5)},
		{name: "no markers and no global is the zero time", item: item("meeting:m1", "user:a"), want: time.Time{}},
		{
			name:    "type marker newer than global wins",
			global:  at(5),
			markers: map[string]time.Time{typeMarkerKey("meeting"): at(9)},
			item:    item("meeting:m1", "user:a"),
			want:    at(9),
		},
		{
			name:    "user marker newer than global wins",
			global:  at(5),
			markers: map[string]time.Time{userMarkerKey("meeting", "user:a"): at(8)},
			item:    item("meeting:m1", "user:a"),
			want:    at(8),
		},
		{
			name:   "the latest of global, type and user wins",
			global: at(5),
			markers: map[string]time.Time{
				typeMarkerKey("meeting"):           at(7),
				userMarkerKey("meeting", "user:a"): at(10),
			},
			item: item("meeting:m1", "user:a"),
			want: at(10),
		},
		{
			name:   "older markers never lower the cutoff",
			global: at(20),
			markers: map[string]time.Time{
				typeMarkerKey("meeting"):           at(1),
				userMarkerKey("meeting", "user:a"): at(2),
			},
			item: item("meeting:m1", "user:a"),
			want: at(20),
		},
		{
			name:    "another type's marker is ignored",
			global:  at(5),
			markers: map[string]time.Time{typeMarkerKey("vote"): at(9)},
			item:    item("meeting:m1", "user:a"),
			want:    at(5),
		},
		{
			name:    "another user's marker is ignored",
			global:  at(5),
			markers: map[string]time.Time{userMarkerKey("meeting", "user:b"): at(9)},
			item:    item("meeting:m1", "user:a"),
			want:    at(5),
		},
		{
			name:    "the same user on another type is ignored",
			global:  at(5),
			markers: map[string]time.Time{userMarkerKey("vote", "user:a"): at(9)},
			item:    item("meeting:m1", "user:a"),
			want:    at(5),
		},
		{
			name:    "a wildcard check consults only the type marker",
			global:  at(5),
			markers: map[string]time.Time{typeMarkerKey("meeting"): at(7), userMarkerKey("meeting", "user:*"): at(9)},
			item:    item("meeting:m1", "user:*"),
			want:    at(7),
		},
		{
			name:    "a malformed object consults only the global time",
			global:  at(5),
			markers: map[string]time.Time{typeMarkerKey("meeting"): at(9)},
			item:    item("meeting", "user:a"),
			want:    at(5),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kv := newFlakyKV()
			for key, created := range tt.markers {
				kv.PutAt(key, []byte("1"), created)
			}
			cutoffs := CacheLayer{bucket: kv}.loadInvalidationCutoffs(ctx, tt.global, []ClientBatchCheckItem{tt.item})
			assert.True(t, tt.want.Equal(cutoffs.forTuple(tt.item)), "want %v got %v", tt.want, cutoffs.forTuple(tt.item))
		})
	}

	t.Run("an unreadable type marker makes that type's entries stale", func(t *testing.T) {
		kv := newFlakyKV()
		kv.failGet = func(key string) error {
			if key == typeMarkerKey("vote") {
				return errors.New("kv glitch")
			}
			return nil
		}
		items := []ClientBatchCheckItem{item("vote:v1", "user:a"), item("meeting:m1", "user:a")}
		cutoffs := CacheLayer{bucket: kv}.loadInvalidationCutoffs(ctx, invalidationTestBase, items)

		assert.True(t, cutoffs.forTuple(items[0]).After(time.Now()), "vote must be treated as stale")
		assert.True(t, cutoffs.forTuple(items[1]).Equal(invalidationTestBase), "meeting must be unaffected")
	})

	t.Run("an unreadable user marker makes that user's entries stale", func(t *testing.T) {
		kv := newFlakyKV()
		kv.failGet = func(key string) error {
			if key == userMarkerKey("meeting", "user:a") {
				return errors.New("kv glitch")
			}
			return nil
		}
		items := []ClientBatchCheckItem{item("meeting:m1", "user:a"), item("meeting:m1", "user:b")}
		cutoffs := CacheLayer{bucket: kv}.loadInvalidationCutoffs(ctx, invalidationTestBase, items)

		assert.True(t, cutoffs.forTuple(items[0]).After(time.Now()))
		assert.True(t, cutoffs.forTuple(items[1]).Equal(invalidationTestBase))
	})

	t.Run("a canceled context marks markers failed instead of hanging", func(t *testing.T) {
		kv := newFlakyKV()
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		// Exhaust the service-wide KV budget so the slot wait observes the
		// canceled context.
		for i := 0; i < cacheOpConcurrency; i++ {
			cacheOpSem <- struct{}{}
		}
		defer func() {
			for i := 0; i < cacheOpConcurrency; i++ {
				<-cacheOpSem
			}
		}()
		it := item("meeting:m1", "user:a")
		cutoffs := CacheLayer{bucket: kv}.loadInvalidationCutoffs(canceled, invalidationTestBase, []ClientBatchCheckItem{it})
		assert.True(t, cutoffs.forTuple(it).After(time.Now()))
	})

	t.Run("markers are fetched once per distinct type and user", func(t *testing.T) {
		kv := newFlakyKV()
		var items []ClientBatchCheckItem
		for i := 0; i < 200; i++ {
			items = append(items, item(fmt.Sprintf("meeting:m%d", i), "user:a"))
		}
		items = append(items, item("vote:v1", "user:a"), item("vote:v2", "user:b"))
		CacheLayer{bucket: kv}.loadInvalidationCutoffs(ctx, invalidationTestBase, items)

		// types: meeting, vote. users: (meeting,a), (vote,a), (vote,b).
		assert.EqualValues(t, 5, kv.gets.Load())
	})

	t.Run("an empty batch reads nothing", func(t *testing.T) {
		kv := newFlakyKV()
		CacheLayer{bucket: kv}.loadInvalidationCutoffs(ctx, invalidationTestBase, nil)
		assert.EqualValues(t, 0, kv.gets.Load())
	})
}

func TestInvalidationCutoffsForTupleWithoutMarkers(t *testing.T) {
	c := invalidationCutoffs{global: invalidationTestBase}
	assert.True(t, c.forTuple(ClientBatchCheckItem{Object: "meeting:m1", User: "user:a"}).Equal(invalidationTestBase))
	assert.True(t, c.forTuple(ClientBatchCheckItem{Object: "garbage", User: "user:a"}).Equal(invalidationTestBase))
}

// ── End to end through FgaService ────────────────────────────────────────────

// fakeClock hands out strictly increasing times, one second apart per call, so
// entries and markers written in sequence have an unambiguous order.
type fakeClock struct {
	mu sync.Mutex
	n  int
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	return invalidationTestBase.Add(time.Duration(c.n) * time.Second)
}

// recordingFga is an IFgaClient that allows every check and records what
// reached OpenFGA.
type recordingFga struct {
	mu         sync.Mutex
	checked    []string
	writes     []ClientWriteRequest
	readTuples []openfga.Tuple
	writeErr   error
}

func (r *recordingFga) Read(context.Context, ClientReadRequest, ClientReadOptions) (*ClientReadResponse, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return &ClientReadResponse{Tuples: r.readTuples}, nil
}

func (r *recordingFga) Write(_ context.Context, req ClientWriteRequest, _ ClientWriteOptions) (*ClientWriteResponse, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.writes = append(r.writes, req)
	return &ClientWriteResponse{}, r.writeErr
}

func (r *recordingFga) BatchCheck(_ context.Context, req ClientBatchCheckRequest) (*openfga.BatchCheckResponse, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make(map[string]openfga.BatchCheckSingleResult, len(req.Checks))
	for _, check := range req.Checks {
		r.checked = append(r.checked, check.Object+"#"+check.Relation+"@"+check.User)
		result[check.CorrelationId] = openfga.BatchCheckSingleResult{Allowed: openfga.PtrBool(true)}
	}
	return &openfga.BatchCheckResponse{Result: &result}, nil
}

func (r *recordingFga) ListObjects(context.Context, ClientListObjectsRequest, ClientListObjectsOptions) (*ClientListObjectsResponse, error) {
	return &ClientListObjectsResponse{}, nil
}

func (r *recordingFga) ReadAuthorizationModel(context.Context) (*openfga.AuthorizationModel, error) {
	return nil, errors.New("not used")
}

func (r *recordingFga) takeChecked() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := sortedStrings(r.checked)
	r.checked = nil
	return out
}

type e2eEnv struct {
	t     *testing.T
	kv    *flakyKV
	fga   *recordingFga
	svc   FgaService
	clock *fakeClock
}

func newE2E(t *testing.T, mode invalidationMode, graph *modelGraph) *e2eEnv {
	t.Helper()
	clock := &fakeClock{}
	kv := newFlakyKV()
	kv.SetClock(clock.Now)
	fga := &recordingFga{}
	return &e2eEnv{
		t: t, kv: kv, fga: fga, clock: clock,
		svc: newFgaService(fga, kv, true).withInvalidation(mode, graph),
	}
}

// seed stores a fresh "true" cache entry for each relation key.
func (e *e2eEnv) seed(relationKeys ...string) {
	e.t.Helper()
	for _, key := range relationKeys {
		_, err := e.kv.Put(context.Background(), "rel."+b32(key), []byte("true"))
		require.NoError(e.t, err)
	}
}

// check runs CheckRelationships and returns the relation keys that had to be
// resolved through OpenFGA, i.e. the ones the cache could not serve.
func (e *e2eEnv) check(relationKeys ...string) []string {
	e.t.Helper()
	reqs, err := e.svc.ExtractCheckRequests([]byte(strings.Join(relationKeys, "\n")))
	require.NoError(e.t, err)
	out, err := e.svc.CheckRelationships(context.Background(), reqs)
	require.NoError(e.t, err)
	require.Len(e.t, strings.Split(string(out), "\n"), len(relationKeys), "every check must be answered")
	return e.fga.takeChecked()
}

// seededUniverse is the set of cached checks every end-to-end scenario starts
// from: two users across the types the scenarios touch.
var seededUniverse = []string{
	"meeting:m1#viewer@user:a",
	"meeting:m2#viewer@user:a",
	"meeting:m1#viewer@user:b",
	"meeting_attachment:x1#viewer@user:a",
	"past_meeting:pm1#viewer@user:a",
	"vote:v1#viewer@user:a",
	"vote:v1#viewer@user:b",
	"committee:c1#writer@user:a",
	"committee:c1#viewer@user:a",
	"project:p1#viewer@user:a",
	"project:p1#viewer@user:b",
	"b2b_org:o1#auditor@user:a",
	"project_application:pa1#viewer@user:a",
	"mentorship_approver_team:t9#member@user:a",
}

func without(all []string, drop ...string) []string {
	dropped := make(map[string]bool, len(drop))
	for _, d := range drop {
		dropped[d] = true
	}
	var out []string
	for _, v := range all {
		if !dropped[v] {
			out = append(out, v)
		}
	}
	return out
}

func TestScopedInvalidationEndToEnd(t *testing.T) {
	ctx := context.Background()
	graph := platformGraph(t)

	tests := []struct {
		name  string
		mode  invalidationMode
		graph *modelGraph
		op    func(t *testing.T, e *e2eEnv)
		// wantStale lists the seeded checks that must be re-resolved through
		// OpenFGA after op; every other seeded check must still come from
		// the cache.
		wantStale []string
	}{
		{
			name:  "removing a meeting participant invalidates only that user's meeting checks",
			mode:  invalidationScoped,
			graph: graph,
			op: func(t *testing.T, e *e2eEnv) {
				require.NoError(t, e.svc.DeleteTuple(ctx, "user:a", "participant", "meeting:m1"))
			},
			wantStale: []string{
				"meeting:m1#viewer@user:a",
				"meeting:m2#viewer@user:a", // same type, same user: per-type scoping, not per-object
				"meeting_attachment:x1#viewer@user:a",
			},
		},
		{
			name:  "adding a vote participant leaves meeting checks alone",
			mode:  invalidationScoped,
			graph: graph,
			op: func(t *testing.T, e *e2eEnv) {
				require.NoError(t, e.svc.WriteTuple(ctx, "user:a", "participant", "vote:v1"))
			},
			wantStale: []string{"vote:v1#viewer@user:a"},
		},
		{
			name:  "removing a committee writer invalidates every type inheriting writer from committees",
			mode:  invalidationScoped,
			graph: graph,
			op: func(t *testing.T, e *e2eEnv) {
				require.NoError(t, e.svc.DeleteTuple(ctx, "user:a", "writer", "committee:c1"))
			},
			wantStale: []string{
				"committee:c1#writer@user:a",
				"committee:c1#viewer@user:a",
				"meeting:m1#viewer@user:a",
				"meeting:m2#viewer@user:a",
				"meeting_attachment:x1#viewer@user:a",
				"past_meeting:pm1#viewer@user:a",
				"vote:v1#viewer@user:a",
			},
		},
		{
			name:  "adding a user to a team invalidates that user across types granting to team members",
			mode:  invalidationScoped,
			graph: graph,
			op: func(t *testing.T, e *e2eEnv) {
				require.NoError(t, e.svc.WriteTuple(ctx, "user:a", "member", "team:t1"))
			},
			wantStale: without(
				seededUniverse,
				"meeting:m1#viewer@user:b", "vote:v1#viewer@user:b", "project:p1#viewer@user:b",
				"mentorship_approver_team:t9#member@user:a", // not reachable from team#member
			),
		},
		{
			name:  "linking a project to a parent invalidates all users on project-inheriting types",
			mode:  invalidationScoped,
			graph: graph,
			op: func(t *testing.T, e *e2eEnv) {
				require.NoError(t, e.svc.WriteTuple(ctx, "project:p0", "parent", "project:p1"))
			},
			wantStale: without(
				seededUniverse,
				"b2b_org:o1#auditor@user:a",
				"project_application:pa1#viewer@user:a",
				"mentorship_approver_team:t9#member@user:a",
			),
		},
		{
			name:  "granting public viewer on a meeting invalidates meetings for every user",
			mode:  invalidationScoped,
			graph: graph,
			op: func(t *testing.T, e *e2eEnv) {
				require.NoError(t, e.svc.WriteTuple(ctx, "user:*", "viewer", "meeting:m1"))
			},
			wantStale: []string{
				"meeting:m1#viewer@user:a",
				"meeting:m2#viewer@user:a",
				"meeting:m1#viewer@user:b",
			},
		},
		{
			name:  "bulk granting many users folds to type markers",
			mode:  invalidationScoped,
			graph: graph,
			op: func(t *testing.T, e *e2eEnv) {
				var writes []ClientTupleKey
				for i := 0; i < 40; i++ {
					writes = append(writes, ClientTupleKey{
						User: fmt.Sprintf("user:bulk%d", i), Relation: "participant", Object: "meeting:m1",
					})
				}
				require.NoError(t, e.svc.WriteTuples(ctx, writes))
			},
			// user:b is not in the write, but the fold widens to the type.
			wantStale: []string{
				"meeting:m1#viewer@user:a",
				"meeting:m2#viewer@user:a",
				"meeting:m1#viewer@user:b",
				"meeting_attachment:x1#viewer@user:a",
			},
		},
		{
			name:  "deleting all of a user's tuples on an object is scoped like any delete",
			mode:  invalidationScoped,
			graph: graph,
			op: func(t *testing.T, e *e2eEnv) {
				e.fga.readTuples = []openfga.Tuple{{Key: openfga.TupleKey{
					User: "user:a", Relation: "participant", Object: "meeting:m1",
				}}}
				require.NoError(t, e.svc.DeleteTuplesByUserAndObject(ctx, "user:a", "meeting:m1"))
			},
			wantStale: []string{
				"meeting:m1#viewer@user:a",
				"meeting:m2#viewer@user:a",
				"meeting_attachment:x1#viewer@user:a",
			},
		},
		{
			name:  "global mode invalidates everything",
			mode:  invalidationGlobal,
			graph: graph,
			op: func(t *testing.T, e *e2eEnv) {
				require.NoError(t, e.svc.DeleteTuple(ctx, "user:a", "participant", "meeting:m1"))
			},
			wantStale: seededUniverse,
		},
		{
			name:  "read-scoped mode still writes the global key",
			mode:  invalidationReadScoped,
			graph: graph,
			op: func(t *testing.T, e *e2eEnv) {
				require.NoError(t, e.svc.DeleteTuple(ctx, "user:a", "participant", "meeting:m1"))
			},
			wantStale: seededUniverse,
		},
		{
			name:  "scoped mode without a model graph falls back to global",
			mode:  invalidationScoped,
			graph: nil,
			op: func(t *testing.T, e *e2eEnv) {
				require.NoError(t, e.svc.DeleteTuple(ctx, "user:a", "participant", "meeting:m1"))
			},
			wantStale: seededUniverse,
		},
		{
			name:  "a relation the model does not define falls back to global",
			mode:  invalidationScoped,
			graph: graph,
			op: func(t *testing.T, e *e2eEnv) {
				require.NoError(t, e.svc.DeleteTuple(ctx, "user:a", "no_such_relation", "meeting:m1"))
			},
			wantStale: seededUniverse,
		},
		{
			name:  "a conditioned tuple falls back to global",
			mode:  invalidationScoped,
			graph: graph,
			op: func(t *testing.T, e *e2eEnv) {
				require.NoError(t, e.svc.WriteTuples(ctx, []ClientTupleKey{{
					User: "user:a", Relation: "participant", Object: "meeting:m1",
					Condition: &openfga.RelationshipCondition{Name: "c"},
				}}))
			},
			wantStale: seededUniverse,
		},
		{
			name:  "a failed OpenFGA write still invalidates, scoped",
			mode:  invalidationScoped,
			graph: graph,
			op: func(t *testing.T, e *e2eEnv) {
				e.fga.writeErr = errors.New("openfga timeout")
				require.Error(t, e.svc.DeleteTuple(ctx, "user:a", "participant", "meeting:m1"))
			},
			wantStale: []string{
				"meeting:m1#viewer@user:a",
				"meeting:m2#viewer@user:a",
				"meeting_attachment:x1#viewer@user:a",
			},
		},
		{
			name:  "scoped markers written by another replica are honored in read-scoped mode",
			mode:  invalidationReadScoped,
			graph: nil,
			op: func(t *testing.T, e *e2eEnv) {
				e.kv.PutAt(userMarkerKey("meeting", "user:a"), []byte("1"), invalidationTestBase.Add(time.Hour))
				e.kv.PutAt(typeMarkerKey("vote"), []byte("1"), invalidationTestBase.Add(time.Hour))
			},
			wantStale: []string{
				"meeting:m1#viewer@user:a",
				"meeting:m2#viewer@user:a",
				"vote:v1#viewer@user:a",
				"vote:v1#viewer@user:b",
			},
		},
		{
			name:  "global mode does not read scoped markers",
			mode:  invalidationGlobal,
			graph: nil,
			op: func(t *testing.T, e *e2eEnv) {
				e.kv.PutAt(userMarkerKey("meeting", "user:a"), []byte("1"), invalidationTestBase.Add(time.Hour))
				e.kv.PutAt(typeMarkerKey("vote"), []byte("1"), invalidationTestBase.Add(time.Hour))
			},
			wantStale: nil,
		},
		{
			name:  "an unreadable type marker only affects that type",
			mode:  invalidationReadScoped,
			graph: nil,
			op: func(t *testing.T, e *e2eEnv) {
				e.kv.failGet = func(key string) error {
					if key == typeMarkerKey("vote") {
						return errors.New("kv glitch")
					}
					return nil
				}
			},
			wantStale: []string{"vote:v1#viewer@user:a", "vote:v1#viewer@user:b"},
		},
		{
			name:  "a scoped marker write failure falls back to global",
			mode:  invalidationScoped,
			graph: graph,
			op: func(t *testing.T, e *e2eEnv) {
				e.kv.failPut = func(key string) error {
					if strings.HasPrefix(key, "inv.") {
						return errors.New("kv put failed")
					}
					return nil
				}
				require.NoError(t, e.svc.DeleteTuple(ctx, "user:a", "participant", "meeting:m1"))
			},
			wantStale: seededUniverse,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newE2E(t, tt.mode, tt.graph)
			e.seed(seededUniverse...)

			tt.op(t, e)
			e.fga.takeChecked() // ignore anything the operation itself checked

			got := e.check(seededUniverse...)
			assert.Equal(t, sortedStrings(tt.wantStale), got)
		})
	}
}

// TestScopedInvalidationKeepsSeededGrantsFresh covers SyncObjectTuples, which
// invalidates and then seeds positive entries for what it just wrote. Those
// entries are newer than the markers and must keep serving from cache while
// older entries for the same user on the same type go stale.
func TestScopedInvalidationKeepsSeededGrantsFresh(t *testing.T) {
	graph := platformGraph(t)
	e := newE2E(t, invalidationScoped, graph)
	e.seed(seededUniverse...)

	_, _, err := e.svc.SyncObjectTuples(context.Background(), "meeting:m1", []ClientTupleKey{
		{User: "user:a", Relation: "participant", Object: "meeting:m1"},
	})
	require.NoError(t, err)

	stale := e.check(append([]string{"meeting:m1#participant@user:a"}, seededUniverse...)...)
	assert.Equal(t, sortedStrings([]string{
		"meeting:m1#viewer@user:a",
		"meeting:m2#viewer@user:a",
		"meeting_attachment:x1#viewer@user:a",
	}), stale, "the seeded participant grant stays cached; older meeting checks for the user go stale")
}

// TestScopedInvalidationRevokesAccessEvenWithSurvivingEntries is the scenario
// that motivates the feature: a cached "true" for a removed participant must
// not be served after removal, while an unrelated user's entry keeps serving.
func TestScopedInvalidationRevokesAccessEvenWithSurvivingEntries(t *testing.T) {
	graph := platformGraph(t)
	e := newE2E(t, invalidationScoped, graph)
	e.seed("meeting:m1#viewer@user:a", "meeting:m1#viewer@user:b", "vote:v1#viewer@user:a")

	require.NoError(t, e.svc.DeleteTuple(context.Background(), "user:a", "participant", "meeting:m1"))

	stale := e.check("meeting:m1#viewer@user:a", "meeting:m1#viewer@user:b", "vote:v1#viewer@user:a")
	assert.Equal(t, []string{"meeting:m1#viewer@user:a"}, stale)

	// The marker written is the narrow one, not the global key.
	assert.False(t, e.kv.written("inv"))
	assert.True(t, e.kv.written(userMarkerKey("meeting", "user:a")))
	assert.True(t, e.kv.written(userMarkerKey("meeting_attachment", "user:a")))
	assert.False(t, e.kv.written(userMarkerKey("vote", "user:a")))
}

// TestScopedInvalidationSequentialWrites checks that markers accumulate
// correctly across writes: a later write must not hide an earlier one.
func TestScopedInvalidationSequentialWrites(t *testing.T) {
	graph := platformGraph(t)
	ctx := context.Background()
	e := newE2E(t, invalidationScoped, graph)

	e.seed("meeting:m1#viewer@user:a", "vote:v1#viewer@user:a", "vote:v1#viewer@user:b")
	require.NoError(t, e.svc.DeleteTuple(ctx, "user:a", "participant", "meeting:m1"))
	require.NoError(t, e.svc.DeleteTuple(ctx, "user:b", "participant", "vote:v1"))

	stale := e.check("meeting:m1#viewer@user:a", "vote:v1#viewer@user:a", "vote:v1#viewer@user:b")
	assert.Equal(t, []string{"meeting:m1#viewer@user:a", "vote:v1#viewer@user:b"}, stale)

	// After the re-check the results are cached again and stay fresh.
	assert.Empty(t, e.check("meeting:m1#viewer@user:a", "vote:v1#viewer@user:a", "vote:v1#viewer@user:b"))
}

func TestConfigureInvalidation(t *testing.T) {
	ctx := context.Background()

	t.Run("global by default", func(t *testing.T) {
		t.Setenv(invalidationModeEnv, "")
		mode, graph := configureInvalidation(ctx, new(MockFgaClient))
		assert.Equal(t, invalidationGlobal, mode)
		assert.Nil(t, graph)
	})

	t.Run("unrecognized value is global", func(t *testing.T) {
		t.Setenv(invalidationModeEnv, "bogus")
		mode, graph := configureInvalidation(ctx, new(MockFgaClient))
		assert.Equal(t, invalidationGlobal, mode)
		assert.Nil(t, graph)
	})

	t.Run("read-scoped does not load the model", func(t *testing.T) {
		t.Setenv(invalidationModeEnv, modeNameReadScoped)
		client := new(MockFgaClient) // any unexpected ReadAuthorizationModel call would panic
		mode, graph := configureInvalidation(ctx, client)
		assert.Equal(t, invalidationReadScoped, mode)
		assert.Nil(t, graph)
	})

	t.Run("scoped loads the model graph", func(t *testing.T) {
		t.Setenv(invalidationModeEnv, modeNameScoped)
		client := new(MockFgaClient)
		client.On("ReadAuthorizationModel", mock.Anything).Return(parseModelDSL(t, platformModelDSL(t)), nil)
		mode, graph := configureInvalidation(ctx, client)
		assert.Equal(t, invalidationScoped, mode)
		require.NotNil(t, graph)
		client.AssertExpectations(t)
	})

	t.Run("scoped keeps the mode but drops the graph when the model cannot be loaded", func(t *testing.T) {
		t.Setenv(invalidationModeEnv, modeNameScoped)
		client := new(MockFgaClient)
		client.On("ReadAuthorizationModel", mock.Anything).Return((*openfga.AuthorizationModel)(nil), errors.New("down"))
		mode, graph := configureInvalidation(ctx, client)
		assert.Equal(t, invalidationScoped, mode, "lookups must still honor scoped markers")
		assert.Nil(t, graph, "writes must stay global without a graph")
	})
}
