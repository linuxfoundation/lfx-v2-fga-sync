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
	"go.opentelemetry.io/otel/codes"
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
		{in: "true", want: invalidationReadScoped, wantOK: false, str: modeNameReadScoped},
		{in: "scopped", want: invalidationReadScoped, wantOK: false, str: modeNameReadScoped}, // cspell:disable-line
		{in: "scoped-writes", want: invalidationReadScoped, wantOK: false, str: modeNameReadScoped},
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
	// hangPut, when it returns true for a key, makes Put block until its
	// context ends, like a NATS write that exhausts its deadline.
	hangPut func(key string) bool
	// hangGet, when it returns true for a key, makes Get block until its
	// context ends.
	hangGet func(key string) bool
	// afterGet, when set, runs after every Get has read its entry and before
	// the caller sees it, so a test can land another replica's write in the
	// window between a read and the conditional write that follows it.
	afterGet func(key string)
	gets     atomic.Int64
	getKeys  sync.Map
}

func (f *flakyKV) Get(ctx context.Context, key string) (jetstream.KeyValueEntry, error) {
	f.gets.Add(1)
	f.getKeys.Store(key, true)
	if f.hangGet != nil && f.hangGet(key) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if f.failGet != nil {
		if err := f.failGet(key); err != nil {
			return nil, err
		}
	}
	entry, err := f.MockKeyValue.Get(ctx, key)
	if f.afterGet != nil {
		f.afterGet(key)
	}
	return entry, err
}

// beforeWrite applies the injected write failures shared by Put, Create and
// Update.
func (f *flakyKV) beforeWrite(ctx context.Context, key string) error {
	if f.hangPut != nil && f.hangPut(key) {
		<-ctx.Done()
		return ctx.Err()
	}
	// A real KV write fails on a finished context.
	if err := ctx.Err(); err != nil {
		return err
	}
	if f.failPut != nil {
		return f.failPut(key)
	}
	return nil
}

func (f *flakyKV) Put(ctx context.Context, key string, value []byte) (uint64, error) {
	if err := f.beforeWrite(ctx, key); err != nil {
		return 0, err
	}
	return f.MockKeyValue.Put(ctx, key, value)
}

func (f *flakyKV) Create(ctx context.Context, key string, value []byte, opts ...jetstream.KVCreateOpt) (uint64, error) {
	if err := f.beforeWrite(ctx, key); err != nil {
		return 0, err
	}
	return f.MockKeyValue.Create(ctx, key, value, opts...)
}

func (f *flakyKV) Update(ctx context.Context, key string, value []byte, revision uint64) (uint64, error) {
	if err := f.beforeWrite(ctx, key); err != nil {
		return 0, err
	}
	return f.MockKeyValue.Update(ctx, key, value, revision)
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

	t.Run("unrecognized value is read-scoped", func(t *testing.T) {
		t.Setenv(invalidationModeEnv, "bogus")
		mode, graph := configureInvalidation(ctx, new(MockFgaClient))
		assert.Equal(t, invalidationReadScoped, mode)
		assert.True(t, invalidationConfig{mode: mode}.readsScoped(), "scoped markers from other replicas must be honored")
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

// TestApplyInvalidationTracesScopedWrites covers the fga_sync.cache.invalidate
// span on the scoped path: it records the marker count, is marked errored when
// a marker write fails, and the global fallback nests its own span under it.
func TestApplyInvalidationTracesScopedWrites(t *testing.T) {
	ctx := context.Background()
	plan := invalidationPlan{
		types: []string{"vote"},
		users: []userMarker{{typ: "meeting", user: "user:a"}},
	}

	t.Run("successful scoped write records the marker count", func(t *testing.T) {
		sr := useRecordingTracer(t)
		require.NoError(t, CacheLayer{bucket: newFlakyKV()}.applyInvalidation(ctx, plan))

		spans := spansNamed(sr, "fga_sync.cache.invalidate")
		require.Len(t, spans, 1)
		assert.Equal(t, int64(len(plan.markerKeys())), spanIntAttrs(spans[0])["fga_sync.cache.invalidate.markers"])
		assert.NotEqual(t, codes.Error, spans[0].Status().Code)
	})

	t.Run("failed marker write marks the scoped span errored and falls back", func(t *testing.T) {
		sr := useRecordingTracer(t)
		kv := newFlakyKV()
		kv.failPut = func(key string) error {
			if key == globalInvalidationKey {
				return nil
			}
			return assert.AnError
		}
		require.NoError(t, CacheLayer{bucket: kv}.applyInvalidation(ctx, plan))
		assert.True(t, kv.written(globalInvalidationKey))

		spans := spansNamed(sr, "fga_sync.cache.invalidate")
		require.Len(t, spans, 2, "scoped attempt plus the global fallback")
		var errored int
		for _, s := range spans {
			if s.Status().Code == codes.Error {
				errored++
			}
		}
		assert.Equal(t, 1, errored)
	})

	t.Run("a global plan emits only the global span", func(t *testing.T) {
		sr := useRecordingTracer(t)
		require.NoError(t, CacheLayer{bucket: newFlakyKV()}.applyInvalidation(ctx, globalPlan()))
		assert.Len(t, spansNamed(sr, "fga_sync.cache.invalidate"), 1)
	})
}

// TestApplyInvalidationFallbackSurvivesExhaustedDeadline covers the case where
// scoped marker writes use up the whole invalidation deadline: the global
// fallback must still get a live context, or revoked grants stay cached.
func TestApplyInvalidationFallbackSurvivesExhaustedDeadline(t *testing.T) {
	kv := newFlakyKV()
	kv.hangPut = func(key string) bool { return key != globalInvalidationKey }

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	plan := invalidationPlan{types: []string{"vote"}}
	require.NoError(t, CacheLayer{bucket: kv}.applyInvalidation(ctx, plan))
	assert.True(t, kv.written(globalInvalidationKey), "the global marker must be written after scoped writes time out")
}

// TestPlanInvalidationTuplesetSubjects covers tuples on a tupleset relation
// (the "owner_user" in "friend from owner_user"). Their subject is the object
// being traversed, so a concrete user there must widen to type markers, while
// a tuple that actually grants a user access stays scoped to that user.
func TestPlanInvalidationTuplesetSubjects(t *testing.T) {
	g := graphFromDSL(t, `model
  schema 1.1
type user
  relations
    define friend: [user]
type doc
  relations
    define owner_user: [user]
    define direct: [user]
    define viewer: friend from owner_user or direct
`)

	tests := []struct {
		name  string
		tuple tupleRef
		want  invalidationPlan
	}{
		{
			name:  "concrete user on a tupleset relation widens to the type",
			tuple: tupleRef{user: "user:a", relation: "owner_user", object: "doc:1"},
			want:  invalidationPlan{types: []string{"doc"}},
		},
		{
			name:  "concrete user on a plain grant relation stays scoped to the user",
			tuple: tupleRef{user: "user:a", relation: "direct", object: "doc:1"},
			want:  invalidationPlan{users: []userMarker{{typ: "doc", user: "user:a"}}},
		},
		{
			name:  "concrete user on the traversed relation stays scoped to the user",
			tuple: tupleRef{user: "user:b", relation: "friend", object: "user:a"},
			want: invalidationPlan{users: []userMarker{
				{typ: "doc", user: "user:b"},
				{typ: "user", user: "user:b"},
			}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, g.planInvalidation([]tupleRef{tt.tuple}))
		})
	}

	t.Run("the platform model has no tupleset that takes a concrete user", func(t *testing.T) {
		platform := platformGraph(t)
		assert.True(t, platform.isTupleset("meeting", "committee"))
		assert.False(t, platform.isTupleset("meeting", "participant"))
	})
}

// TestApplyInvalidationRecoversMissingBucket covers a replica that started
// without the cache bucket: once the bucket exists, every plan kind must write
// its markers to it, because other replicas may be caching results there.
func TestApplyInvalidationRecoversMissingBucket(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name     string
		plan     invalidationPlan
		wantPuts []string
	}{
		{name: "global plan", plan: invalidationPlan{global: true, reason: globalReasonModeGlobal}, wantPuts: []string{"inv"}},
		{name: "scoped plan", plan: invalidationPlan{types: []string{"meeting"}}, wantPuts: []string{"inv.t.meeting"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kv := &keyRecordingKV{}
			var bound INatsKeyValue
			cache := CacheLayer{recovery: &bucketRecovery{bind: func(context.Context) (INatsKeyValue, error) {
				if bound == nil {
					return nil, jetstream.ErrBucketNotFound
				}
				return bound, nil
			}}}

			require.NoError(t, cache.applyInvalidation(ctx, tt.plan), "a still-missing bucket is not an error")
			assert.Empty(t, kv.puts)

			bound = kv
			require.NoError(t, cache.applyInvalidation(ctx, tt.plan))
			assert.Equal(t, tt.wantPuts, kv.puts)
			assert.Nil(t, cache.bucket, "reads stay disabled after recovery")
		})
	}
}

func TestCheckModelMarker(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name       string
		modelID    string
		marker     string // empty means the marker key does not exist
		getErr     error
		wantReason string
		wantGets   int64
	}{
		{name: "no model ID skips the check", modelID: "", marker: "m-old", wantReason: "", wantGets: 0},
		{name: "missing marker is a match", modelID: "m-new", wantReason: "", wantGets: 1},
		{name: "matching marker", modelID: "m-new", marker: "m-new", wantReason: "", wantGets: 1},
		{name: "different model announced", modelID: "m-old", marker: "m-new", wantReason: globalReasonModelMismatch, wantGets: 1},
		{
			name: "marker read failure", modelID: "m-new", getErr: errors.New("kv down"),
			wantReason: globalReasonModelCheckFailed, wantGets: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kv := newFlakyKV()
			if tt.marker != "" {
				_, err := kv.Put(ctx, modelMarkerKey, []byte(tt.marker))
				require.NoError(t, err)
			}
			if tt.getErr != nil {
				kv.failGet = func(string) error { return tt.getErr }
			}
			assert.Equal(t, tt.wantReason, checkModelMarker(ctx, kv, tt.modelID))
			assert.Equal(t, tt.wantGets, kv.gets.Load())
		})
	}
}

func TestApplyInvalidationModelGuard(t *testing.T) {
	ctx := context.Background()
	scopedPlan := invalidationPlan{types: []string{"meeting"}, modelID: "m-old"}

	t.Run("a different announced model makes the write global", func(t *testing.T) {
		kv := newFlakyKV()
		_, err := kv.Put(ctx, modelMarkerKey, []byte("m-new"))
		require.NoError(t, err)
		require.NoError(t, CacheLayer{bucket: kv}.applyInvalidation(ctx, scopedPlan))
		assert.True(t, kv.written(globalInvalidationKey))
		assert.False(t, kv.written(typeMarkerKey("meeting")), "no scoped marker from an outdated graph")
	})

	t.Run("a matching model stays scoped", func(t *testing.T) {
		kv := newFlakyKV()
		_, err := kv.Put(ctx, modelMarkerKey, []byte("m-old"))
		require.NoError(t, err)
		require.NoError(t, CacheLayer{bucket: kv}.applyInvalidation(ctx, scopedPlan))
		assert.True(t, kv.written(typeMarkerKey("meeting")))
		assert.False(t, kv.written(globalInvalidationKey))
	})

	t.Run("an unreadable marker makes the write global", func(t *testing.T) {
		kv := newFlakyKV()
		kv.failGet = func(key string) error {
			if key == modelMarkerKey {
				return errors.New("kv down")
			}
			return nil
		}
		require.NoError(t, CacheLayer{bucket: kv}.applyInvalidation(ctx, scopedPlan))
		assert.True(t, kv.written(globalInvalidationKey))
		assert.False(t, kv.written(typeMarkerKey("meeting")))
	})

	t.Run("a marker read that exhausts the deadline still invalidates globally", func(t *testing.T) {
		kv := newFlakyKV()
		kv.hangGet = func(key string) bool { return key == modelMarkerKey }
		deadlineCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		defer cancel()
		require.NoError(t, CacheLayer{bucket: kv}.applyInvalidation(deadlineCtx, scopedPlan))
		assert.True(t, kv.written(globalInvalidationKey), "the fallback runs on its own deadline")
		assert.False(t, kv.written(typeMarkerKey("meeting")))
	})

	t.Run("a global plan never reads the marker", func(t *testing.T) {
		kv := newFlakyKV()
		require.NoError(t, CacheLayer{bucket: kv}.applyInvalidation(ctx, globalPlanFor(globalReasonModeGlobal)))
		assert.True(t, kv.written(globalInvalidationKey))
		assert.Zero(t, kv.gets.Load())
	})
}

func TestAnnounceModel(t *testing.T) {
	ctx := context.Background()
	marker := func(t *testing.T, kv *flakyKV, id string) {
		t.Helper()
		_, err := kv.Put(ctx, modelMarkerKey, []byte(id))
		require.NoError(t, err)
	}
	markerValue := func(t *testing.T, kv *flakyKV) string {
		t.Helper()
		entry, err := kv.Get(ctx, modelMarkerKey)
		require.NoError(t, err)
		return string(entry.Value())
	}

	t.Run("flushes, then writes the marker", func(t *testing.T) {
		kv := &keyRecordingKV{}
		flushed, err := CacheLayer{bucket: kv}.announceModel(ctx, "m-2")
		require.NoError(t, err)
		assert.True(t, flushed)
		assert.Equal(t, []string{globalInvalidationKey, modelMarkerKey}, kv.puts,
			"the marker goes last, so it only ever means the flush completed")
	})

	t.Run("an older marker is replaced and the cache flushed", func(t *testing.T) {
		kv := newFlakyKV()
		marker(t, kv, "m-1")
		flushed, err := CacheLayer{bucket: kv}.announceModel(ctx, "m-2")
		require.NoError(t, err)
		assert.True(t, flushed)
		assert.Equal(t, "m-2", markerValue(t, kv))
		assert.True(t, kv.written(globalInvalidationKey))
	})

	t.Run("the same model only refreshes the marker", func(t *testing.T) {
		kv := newFlakyKV()
		marker(t, kv, "m-2")
		flushed, err := CacheLayer{bucket: kv}.announceModel(ctx, "m-2")
		require.NoError(t, err)
		assert.False(t, flushed, "a pod restart must not flush the whole cache")
		assert.False(t, kv.written(globalInvalidationKey))
		assert.Equal(t, "m-2", markerValue(t, kv))
	})

	t.Run("a newer marker is left alone", func(t *testing.T) {
		kv := newFlakyKV()
		marker(t, kv, "m-2")
		flushed, err := CacheLayer{bucket: kv}.announceModel(ctx, "m-1")
		require.ErrorIs(t, err, errOutdatedModel)
		assert.False(t, flushed)
		assert.Equal(t, "m-2", markerValue(t, kv), "an outdated replica must not take the marker back")
		assert.False(t, kv.written(globalInvalidationKey))
	})

	t.Run("an unreadable marker is an error and writes nothing", func(t *testing.T) {
		kv := newFlakyKV()
		kv.failGet = func(string) error { return errors.New("kv down") }
		_, err := CacheLayer{bucket: kv}.announceModel(ctx, "m-2")
		require.ErrorContains(t, err, "reading model marker")
		assert.False(t, kv.written(modelMarkerKey))
		assert.False(t, kv.written(globalInvalidationKey))
	})

	t.Run("flushes and reports a marker write failure", func(t *testing.T) {
		kv := newFlakyKV()
		kv.failPut = func(key string) error {
			if key == modelMarkerKey {
				return errors.New("kv down")
			}
			return nil
		}
		flushed, err := CacheLayer{bucket: kv}.announceModel(ctx, "m-2")
		require.ErrorContains(t, err, "writing model marker")
		assert.True(t, flushed)
		assert.True(t, kv.written(globalInvalidationKey), "the flush still runs")
	})

	// racing lands another replica's marker write between the announcement's
	// read and its conditional write, by running after the nth Get of the marker.
	racing := func(kv *flakyKV, writes ...string) {
		var gets atomic.Int64
		kv.afterGet = func(key string) {
			if key != modelMarkerKey {
				return
			}
			if n := int(gets.Add(1)); n <= len(writes) {
				_, err := kv.MockKeyValue.Put(ctx, modelMarkerKey, []byte(writes[n-1]))
				require.NoError(t, err)
			}
		}
	}

	t.Run("a newer announcement landing after a missing-marker read is not overwritten", func(t *testing.T) {
		kv := newFlakyKV()
		racing(kv, "m-3")
		flushed, err := CacheLayer{bucket: kv}.announceModel(ctx, "m-2")
		require.ErrorIs(t, err, errOutdatedModel)
		assert.False(t, flushed)
		assert.Equal(t, "m-3", markerValue(t, kv), "the flush made before losing the race is harmless")
	})

	t.Run("a newer announcement landing after an older-marker read is not overwritten", func(t *testing.T) {
		kv := newFlakyKV()
		marker(t, kv, "m-1")
		racing(kv, "m-3")
		_, err := CacheLayer{bucket: kv}.announceModel(ctx, "m-2")
		require.ErrorIs(t, err, errOutdatedModel)
		assert.Equal(t, "m-3", markerValue(t, kv))
	})

	t.Run("a newer announcement landing after a same-model read is not overwritten by the refresh", func(t *testing.T) {
		kv := newFlakyKV()
		marker(t, kv, "m-2")
		racing(kv, "m-3")
		flushed, err := CacheLayer{bucket: kv}.announceModel(ctx, "m-2")
		require.ErrorIs(t, err, errOutdatedModel)
		assert.False(t, flushed)
		assert.Equal(t, "m-3", markerValue(t, kv))
	})

	t.Run("an older announcement landing first is replaced after re-reading", func(t *testing.T) {
		kv := newFlakyKV()
		racing(kv, "m-1")
		flushed, err := CacheLayer{bucket: kv}.announceModel(ctx, "m-2")
		require.NoError(t, err)
		assert.True(t, flushed)
		assert.Equal(t, "m-2", markerValue(t, kv))
		assert.True(t, kv.written(globalInvalidationKey))
	})

	t.Run("gives up when the marker keeps changing", func(t *testing.T) {
		kv := newFlakyKV()
		writes := make([]string, modelMarkerWriteAttempts)
		for i := range writes {
			writes[i] = "m-1"
		}
		racing(kv, writes...)
		_, err := CacheLayer{bucket: kv}.announceModel(ctx, "m-2")
		require.ErrorContains(t, err, "kept changing")
	})

	t.Run("a failed flush leaves nothing announced", func(t *testing.T) {
		kv := newFlakyKV()
		kv.failPut = func(key string) error {
			if key == globalInvalidationKey {
				return errors.New("kv down")
			}
			return nil
		}
		flushed, err := CacheLayer{bucket: kv}.announceModel(ctx, "m-2")
		require.ErrorContains(t, err, "flushing the cache")
		assert.False(t, flushed)
		assert.False(t, kv.written(modelMarkerKey), "a marker without its flush would let later replicas skip it")
	})

	t.Run("is a no-op while no bucket exists", func(t *testing.T) {
		cache := CacheLayer{recovery: &bucketRecovery{bind: func(context.Context) (INatsKeyValue, error) {
			return nil, jetstream.ErrBucketNotFound
		}}}
		flushed, err := cache.announceModel(ctx, "m-2")
		require.NoError(t, err)
		assert.False(t, flushed)
	})
}

func TestFollowUpFlush(t *testing.T) {
	t.Run("bumps the global key after the delay", func(t *testing.T) {
		kv := newFlakyKV()
		CacheLayer{bucket: kv}.followUpFlush(context.Background(), time.Millisecond)
		assert.Eventually(t, func() bool { return kv.written(globalInvalidationKey) }, time.Second, time.Millisecond)
	})

	t.Run("gives up when the service is shutting down", func(t *testing.T) {
		kv := &keyRecordingKV{}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		CacheLayer{bucket: kv}.followUpFlush(ctx, time.Millisecond)
		time.Sleep(50 * time.Millisecond)
		kv.mu.Lock()
		defer kv.mu.Unlock()
		assert.Empty(t, kv.puts)
	})
}

func TestFgaServiceModelGuard(t *testing.T) {
	ctx := context.Background()
	graph := platformGraph(t)
	newSvc := func(kv INatsKeyValue, mode invalidationMode, g *modelGraph) FgaService {
		return newFgaService(&recordingFga{}, kv, true).withInvalidation(mode, g).withModelGuard("m-1")
	}

	t.Run("scoped plans carry the model ID and global plans do not", func(t *testing.T) {
		svc := newSvc(newFlakyKV(), invalidationScoped, graph)
		scoped := svc.invalidationPlan([]ClientTupleKey{{User: "user:a", Relation: "participant", Object: "meeting:m1"}}, nil)
		require.False(t, scoped.global)
		assert.Equal(t, "m-1", scoped.modelID)

		conditioned := svc.invalidationPlan([]ClientTupleKey{{
			User: "user:a", Relation: "participant", Object: "meeting:m1",
			Condition: &openfga.RelationshipCondition{Name: "c"},
		}}, nil)
		require.True(t, conditioned.global)
		assert.Empty(t, conditioned.modelID)
	})

	t.Run("announces in scoped mode whether or not the graph loaded", func(t *testing.T) {
		for _, tt := range []struct {
			name     string
			mode     invalidationMode
			graph    *modelGraph
			wantPuts []string
		}{
			{name: "global mode", mode: invalidationGlobal, graph: graph},
			{name: "read-scoped mode", mode: invalidationReadScoped, graph: graph},
			{name: "scoped without a graph", mode: invalidationScoped, graph: nil, wantPuts: []string{globalInvalidationKey, modelMarkerKey}},
			{name: "scoped", mode: invalidationScoped, graph: graph, wantPuts: []string{globalInvalidationKey, modelMarkerKey}},
		} {
			t.Run(tt.name, func(t *testing.T) {
				kv := &keyRecordingKV{}
				runCtx, cancel := context.WithCancel(ctx)
				defer cancel() // stops the follow-up flush
				require.NoError(t, newSvc(kv, tt.mode, tt.graph).announceModel(runCtx))
				assert.Equal(t, tt.wantPuts, kv.puts)
			})
		}
	})
}

func TestAnnounceModelRetries(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // stops the follow-up flush
	prev := modelAnnounceRetryDelay
	modelAnnounceRetryDelay = time.Millisecond
	t.Cleanup(func() { modelAnnounceRetryDelay = prev })
	graph := platformGraph(t)

	t.Run("succeeds once the marker write recovers", func(t *testing.T) {
		kv := newFlakyKV()
		var failures atomic.Int64
		kv.failPut = func(key string) error {
			if key == modelMarkerKey && failures.Add(1) <= 2 {
				return errors.New("kv down")
			}
			return nil
		}
		svc := newFgaService(&recordingFga{}, kv, true).withInvalidation(invalidationScoped, graph).withModelGuard("m-2")
		require.NoError(t, svc.announceModel(ctx))
		assert.Equal(t, int64(3), failures.Load())
		assert.True(t, kv.written(modelMarkerKey))
	})

	t.Run("retries the whole announcement when the flush fails", func(t *testing.T) {
		kv := newFlakyKV()
		var markerWrites, flushAttempts atomic.Int64
		kv.failPut = func(key string) error {
			switch key {
			case modelMarkerKey:
				markerWrites.Add(1)
			case globalInvalidationKey:
				if flushAttempts.Add(1) == 1 {
					return errors.New("kv down")
				}
			}
			return nil
		}
		svc := newFgaService(&recordingFga{}, kv, true).withInvalidation(invalidationScoped, graph).withModelGuard("m-2")
		require.NoError(t, svc.announceModel(ctx))
		assert.True(t, kv.written(globalInvalidationKey), "the flush that failed is retried, not skipped")
		assert.Equal(t, int64(1), markerWrites.Load(), "the marker is only written once the flush has succeeded")
		assert.Equal(t, int64(2), flushAttempts.Load())
	})

	t.Run("gives up after the configured attempts", func(t *testing.T) {
		kv := newFlakyKV()
		var attempts atomic.Int64
		kv.failPut = func(key string) error {
			if key == modelMarkerKey {
				attempts.Add(1)
				return errors.New("kv down")
			}
			return nil
		}
		svc := newFgaService(&recordingFga{}, kv, true).withInvalidation(invalidationScoped, graph).withModelGuard("m-2")
		require.Error(t, svc.announceModel(ctx))
		assert.Equal(t, int64(modelAnnounceAttempts), attempts.Load())
	})

	t.Run("does not retry an outdated model", func(t *testing.T) {
		kv := newFlakyKV()
		_, err := kv.Put(ctx, modelMarkerKey, []byte("m-9"))
		require.NoError(t, err)
		svc := newFgaService(&recordingFga{}, kv, true).withInvalidation(invalidationScoped, graph).withModelGuard("m-2")
		require.ErrorIs(t, svc.announceModel(ctx), errOutdatedModel)
		assert.Equal(t, int64(1), kv.gets.Load())
	})
}

// TestAnnounceModelOrDegrade covers what a replica does with the announcement's
// outcome. In every case access checks keep being answered.
func TestAnnounceModelOrDegrade(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // stops the follow-up flush
	prev := modelAnnounceRetryDelay
	modelAnnounceRetryDelay = time.Millisecond
	t.Cleanup(func() { modelAnnounceRetryDelay = prev })
	graph := platformGraph(t)
	tuple := []ClientTupleKey{{User: "user:a", Relation: "participant", Object: "meeting:m1"}}

	t.Run("a successful announcement changes nothing", func(t *testing.T) {
		svc := newFgaService(&recordingFga{}, newFlakyKV(), true).withInvalidation(invalidationScoped, graph).withModelGuard("m-2")
		got := svc.announceModelOrDegrade(ctx)
		assert.True(t, got.useCache)
		assert.NotNil(t, got.cache.bucket)
		assert.False(t, got.invalidationPlan(tuple, nil).global, "still scoping")
	})

	t.Run("an outdated replica writes globally and keeps using the cache", func(t *testing.T) {
		kv := newFlakyKV()
		_, err := kv.Put(ctx, modelMarkerKey, []byte("m-9"))
		require.NoError(t, err)
		svc := newFgaService(&recordingFga{}, kv, true).withInvalidation(invalidationScoped, graph).withModelGuard("m-2")
		got := svc.announceModelOrDegrade(ctx)
		assert.True(t, got.useCache)
		assert.NotNil(t, got.cache.bucket)
		assert.True(t, got.invalidationPlan(tuple, nil).global)
	})

	t.Run("an unrecorded announcement stops using the shared cache but still invalidates it", func(t *testing.T) {
		kv := newFlakyKV()
		kv.failPut = func(key string) error {
			if key == modelMarkerKey {
				return errors.New("kv down")
			}
			return nil
		}
		fga := &recordingFga{}
		svc := newFgaService(fga, kv, true).withInvalidation(invalidationScoped, graph).withModelGuard("m-2")
		got := svc.announceModelOrDegrade(ctx)

		assert.False(t, got.useCache)
		assert.Nil(t, got.cache.bucket, "no reads, write-backs or seeding")
		assert.True(t, got.invalidationPlan(tuple, nil).global)

		// Access checks are still answered, by OpenFGA, and do not touch the cache.
		reqs, err := got.ExtractCheckRequests([]byte("meeting:m1#viewer@user:a"))
		require.NoError(t, err)
		kv.gets.Store(0)
		out, err := got.CheckRelationships(ctx, reqs)
		require.NoError(t, err)
		assert.Contains(t, string(out), "meeting:m1#viewer@user:a")
		assert.Equal(t, []string{"meeting:m1#viewer@user:a"}, fga.takeChecked())
		assert.Zero(t, kv.gets.Load())
		assert.False(t, kv.written("rel."+b32("meeting:m1#viewer@user:a")), "no write-back")

		// Writes still invalidate the bucket other replicas read.
		kv.failPut = nil
		require.NoError(t, got.WriteTuples(ctx, tuple))
		assert.True(t, kv.written(globalInvalidationKey))
	})
}

// TestFailedFlushIsNotHiddenFromLaterReplicas covers a replica whose flush fails
// on every attempt. It must not leave a marker behind, or a later replica on the
// same model would read it, skip the flush and enable the shared cache over
// entries nobody cleared.
func TestFailedFlushIsNotHiddenFromLaterReplicas(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // stops the watchers and follow-up flushes
	prevFence, prevRetry := modelFenceInterval, modelAnnounceRetryDelay
	modelFenceInterval, modelAnnounceRetryDelay = time.Hour, time.Millisecond
	t.Cleanup(func() { modelFenceInterval, modelAnnounceRetryDelay = prevFence, prevRetry })
	graph := platformGraph(t)

	kv := newFlakyKV()
	var flushing atomic.Bool
	kv.failPut = func(key string) error {
		if key == globalInvalidationKey && !flushing.Load() {
			return errors.New("kv down")
		}
		return nil
	}
	first := newFgaService(&recordingFga{}, kv, true).withInvalidation(invalidationScoped, graph).withModelGuard("m-2").
		announceModelOrDegrade(ctx)
	assert.False(t, first.useCache, "the first replica could not announce and stops using the shared cache")
	assert.False(t, kv.written(modelMarkerKey), "no marker without a completed flush")

	flushing.Store(true) // the bucket recovers
	second := newFgaService(&recordingFga{}, kv, true).withInvalidation(invalidationScoped, graph).withModelGuard("m-2").
		announceModelOrDegrade(ctx)
	assert.True(t, kv.written(globalInvalidationKey), "the later replica does the flush")
	assert.True(t, kv.written(modelMarkerKey))
	assert.True(t, second.useCache)
	assert.NotNil(t, second.cache.bucket, "and may use the shared cache")
}

// TestModelRolloutDoesNotLeaveStaleGrants plays a model bump: the replica on
// the old model starts writing globally as soon as the new one announces, so
// its writes cannot leave entries a scoped marker from its outdated graph
// would miss, and restarting it afterwards cannot take the marker back.
func TestModelRolloutDoesNotLeaveStaleGrants(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // stops the follow-up flush
	graph := platformGraph(t)

	e := newE2E(t, invalidationScoped, graph)
	oldSvc := e.svc.withModelGuard("m-1")
	newSvc := newFgaService(e.fga, e.kv, true).withInvalidation(invalidationScoped, graph).withModelGuard("m-2")
	require.NoError(t, oldSvc.announceModel(ctx))

	staleAfterRevoke := func(svc FgaService) []string {
		e.seed(seededUniverse...)
		e.svc = svc
		require.NoError(t, e.svc.DeleteTuple(ctx, "user:a", "participant", "meeting:m1"))
		e.fga.takeChecked()
		return e.check(seededUniverse...)
	}

	assert.NotEqual(t, sortedStrings(seededUniverse), staleAfterRevoke(oldSvc),
		"before the new model appears the write is scoped")

	require.NoError(t, newSvc.announceModel(ctx))
	assert.Equal(t, sortedStrings(seededUniverse), staleAfterRevoke(oldSvc),
		"after the announcement the old replica writes globally")
	assert.NotEqual(t, sortedStrings(seededUniverse), staleAfterRevoke(newSvc), "the new replica keeps scoping")

	// The old replica restarts after the new one: it must not take the marker back.
	require.ErrorIs(t, oldSvc.announceModel(ctx), errOutdatedModel)
	assert.Equal(t, sortedStrings(seededUniverse), staleAfterRevoke(oldSvc),
		"an old replica restarting mid-rollout still writes globally")
}

func TestModelFenceRefresh(t *testing.T) {
	ctx := context.Background()
	put := func(t *testing.T, kv *flakyKV, id string) {
		t.Helper()
		_, err := kv.MockKeyValue.Put(ctx, modelMarkerKey, []byte(id))
		require.NoError(t, err)
	}

	t.Run("a nil fence is not blocked", func(t *testing.T) {
		var f *modelFence
		assert.False(t, f.isBlocked())
	})

	t.Run("follows the announced model", func(t *testing.T) {
		kv := newFlakyKV()
		f := &modelFence{}

		f.refresh(ctx, kv, "m-1")
		assert.False(t, f.isBlocked(), "no marker: nobody has announced")

		put(t, kv, "m-1")
		f.refresh(ctx, kv, "m-1")
		assert.False(t, f.isBlocked(), "the announced model is this replica's")

		put(t, kv, "m-2")
		f.refresh(ctx, kv, "m-1")
		assert.True(t, f.isBlocked(), "a newer model was announced")

		put(t, kv, "m-1")
		f.refresh(ctx, kv, "m-1")
		assert.False(t, f.isBlocked(), "the fence lifts when the announced model matches again")
	})

	t.Run("an unreadable marker blocks until it can be read", func(t *testing.T) {
		kv := newFlakyKV()
		put(t, kv, "m-1")
		kv.failGet = func(string) error { return errors.New("kv down") }
		f := &modelFence{}
		f.refresh(ctx, kv, "m-1")
		assert.True(t, f.isBlocked())

		kv.failGet = nil
		f.refresh(ctx, kv, "m-1")
		assert.False(t, f.isBlocked())
	})

	t.Run("watch picks up a later announcement and stops with its context", func(t *testing.T) {
		kv := newFlakyKV()
		put(t, kv, "m-1")
		f := &modelFence{}
		watchCtx, cancel := context.WithCancel(ctx)
		go f.watch(watchCtx, kv, "m-1", time.Millisecond)

		put(t, kv, "m-2")
		assert.Eventually(t, f.isBlocked, time.Second, time.Millisecond)

		cancel()
		time.Sleep(20 * time.Millisecond) // let an in-flight tick finish
		put(t, kv, "m-1")
		time.Sleep(20 * time.Millisecond)
		assert.True(t, f.isBlocked(), "a stopped watcher no longer updates the fence")
	})
}

// TestModelFenceKeepsOutdatedReplicaFromRepopulatingCache plays the rollout gap
// the flushes cannot cover: an old-model replica that keeps answering checks
// after a new model is announced. It must keep serving, from the cache and from
// OpenFGA, without writing anything back for new-model replicas to read.
func TestModelFenceKeepsOutdatedReplicaFromRepopulatingCache(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // stops the watchers and follow-up flushes
	prevFence, prevRetry := modelFenceInterval, modelAnnounceRetryDelay
	modelFenceInterval, modelAnnounceRetryDelay = time.Millisecond, time.Millisecond
	t.Cleanup(func() { modelFenceInterval, modelAnnounceRetryDelay = prevFence, prevRetry })
	graph := platformGraph(t)

	cached := "meeting:m1#viewer@user:a"
	uncached := "meeting:m2#viewer@user:a"
	rel := func(key string) string { return "rel." + b32(key) }

	e := newE2E(t, invalidationScoped, graph)
	oldSvc := newFgaService(e.fga, e.kv, true).withInvalidation(invalidationScoped, graph).withModelGuard("m-1")
	oldSvc = oldSvc.announceModelOrDegrade(ctx)
	require.False(t, oldSvc.cache.fence.isBlocked(), "the announcing replica may write")

	e.svc = oldSvc
	e.seed(cached)
	assert.Equal(t, []string{uncached}, e.check(cached, uncached), "before the new model, a miss is written back")
	assert.True(t, e.kv.written(rel(uncached)))

	// A new model is announced; the old replica notices within a poll.
	newSvc := newFgaService(e.fga, e.kv, true).withInvalidation(invalidationScoped, graph).withModelGuard("m-2")
	newSvc = newSvc.announceModelOrDegrade(ctx)
	require.False(t, newSvc.cache.fence.isBlocked())
	require.Eventually(t, oldSvc.cache.fence.isBlocked, time.Second, time.Millisecond)

	// The new model's flush made every older entry stale. Leave the old replica
	// one fresh entry to hit, and drop the stale write-back so a new one would
	// be visible.
	e.kv.MockKeyValue.PutAt(rel(cached), []byte("true"), invalidationTestBase.Add(time.Hour))
	e.kv.MockKeyValue.mu.Lock() // the fence watcher is reading the same store
	delete(e.kv.MockKeyValue.data, rel(uncached))
	e.kv.MockKeyValue.mu.Unlock()

	e.svc = oldSvc
	got := e.check(cached, uncached)
	assert.Equal(t, []string{uncached}, got, "the outdated replica still answers misses from OpenFGA, and hits from the cache")
	assert.False(t, e.kv.written(rel(uncached)), "but writes nothing back")

	// The new-model replica is unaffected.
	e.svc = newSvc
	e.check(uncached)
	assert.True(t, e.kv.written(rel(uncached)))
}

// TestAnnounceModelOrDegradeFencesOutdatedReplica covers a replica that starts
// after a newer model was announced: it is fenced from the start.
func TestAnnounceModelOrDegradeFencesOutdatedReplica(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	kv := newFlakyKV()
	_, err := kv.Put(ctx, modelMarkerKey, []byte("m-9"))
	require.NoError(t, err)
	svc := newFgaService(&recordingFga{}, kv, true).
		withInvalidation(invalidationScoped, platformGraph(t)).withModelGuard("m-2")

	got := svc.announceModelOrDegrade(ctx)

	assert.True(t, got.cache.fence.isBlocked())
	assert.True(t, got.useCache, "it still reads from the cache")
	assert.NotNil(t, got.cache.bucket)
}

// expire removes a key the way the bucket TTL would.
func (f *flakyKV) expire(key string) {
	f.MockKeyValue.mu.Lock()
	defer f.MockKeyValue.mu.Unlock()
	delete(f.MockKeyValue.data, key)
}

func TestModelFenceStaysClosedWhenMarkerExpires(t *testing.T) {
	ctx := context.Background()
	kv := newFlakyKV()
	f := &modelFence{}
	_, err := kv.Put(ctx, modelMarkerKey, []byte("m-2"))
	require.NoError(t, err)

	f.refresh(ctx, kv, "m-1")
	require.True(t, f.isBlocked(), "a different model is announced")

	kv.expire(modelMarkerKey)
	f.refresh(ctx, kv, "m-1")
	assert.True(t, f.isBlocked(), "an expired marker must not reopen a replica that saw another model")

	_, err = kv.Put(ctx, modelMarkerKey, []byte("m-1"))
	require.NoError(t, err)
	f.refresh(ctx, kv, "m-1")
	assert.False(t, f.isBlocked(), "only this replica's own model lifts the fence")

	kv.expire(modelMarkerKey)
	f.refresh(ctx, kv, "m-1")
	assert.False(t, f.isBlocked(), "having matched, a later missing marker is not a different model")
}

func TestKeepModelMarkerAlive(t *testing.T) {
	ctx := context.Background()
	revision := func(t *testing.T, kv *flakyKV) uint64 {
		t.Helper()
		entry, err := kv.Get(ctx, modelMarkerKey)
		require.NoError(t, err)
		return entry.Revision()
	}

	t.Run("renews the replica's own marker", func(t *testing.T) {
		kv := newFlakyKV()
		_, err := kv.Put(ctx, modelMarkerKey, []byte("m-1"))
		require.NoError(t, err)
		before := revision(t, kv)
		keepModelMarkerAlive(ctx, kv, "m-1")
		assert.Greater(t, revision(t, kv), before)
	})

	t.Run("leaves another model's marker alone", func(t *testing.T) {
		kv := newFlakyKV()
		_, err := kv.Put(ctx, modelMarkerKey, []byte("m-2"))
		require.NoError(t, err)
		before := revision(t, kv)
		keepModelMarkerAlive(ctx, kv, "m-1")
		assert.Equal(t, before, revision(t, kv))
	})

	t.Run("does not recreate a missing marker", func(t *testing.T) {
		kv := newFlakyKV()
		keepModelMarkerAlive(ctx, kv, "m-1")
		assert.False(t, kv.written(modelMarkerKey))
	})

	t.Run("a lost race is not an error", func(t *testing.T) {
		kv := newFlakyKV()
		_, err := kv.Put(ctx, modelMarkerKey, []byte("m-1"))
		require.NoError(t, err)
		kv.afterGet = func(key string) {
			if key == modelMarkerKey {
				_, putErr := kv.MockKeyValue.Put(ctx, modelMarkerKey, []byte("m-2"))
				require.NoError(t, putErr)
			}
		}
		keepModelMarkerAlive(ctx, kv, "m-1")
		entry, err := kv.MockKeyValue.Get(ctx, modelMarkerKey)
		require.NoError(t, err)
		assert.Equal(t, "m-2", string(entry.Value()), "the newer announcement is not overwritten")
	})

	t.Run("watch renews it on an interval", func(t *testing.T) {
		prev := modelMarkerKeepAliveInterval
		modelMarkerKeepAliveInterval = 5 * time.Millisecond
		t.Cleanup(func() { modelMarkerKeepAliveInterval = prev })

		kv := newFlakyKV()
		_, err := kv.Put(ctx, modelMarkerKey, []byte("m-1"))
		require.NoError(t, err)
		before := revision(t, kv)
		watchCtx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			defer close(done)
			(&modelFence{}).watch(watchCtx, kv, "m-1", time.Millisecond)
		}()
		defer func() { // runs before the cleanup above restores the interval
			cancel()
			<-done
		}()
		assert.Eventually(t, func() bool {
			entry, getErr := kv.MockKeyValue.Get(ctx, modelMarkerKey)
			return getErr == nil && entry.Revision() > before
		}, time.Second, time.Millisecond)
	})
}

func TestFollowUpFlushRetries(t *testing.T) {
	prev := modelAnnounceRetryDelay
	modelAnnounceRetryDelay = time.Millisecond
	t.Cleanup(func() { modelAnnounceRetryDelay = prev })

	t.Run("retries a failed flush until it succeeds", func(t *testing.T) {
		kv := newFlakyKV()
		var attempts atomic.Int64
		kv.failPut = func(key string) error {
			if key == globalInvalidationKey && attempts.Add(1) == 1 {
				return errors.New("kv down")
			}
			return nil
		}
		CacheLayer{bucket: kv}.followUpFlush(context.Background(), time.Millisecond)
		assert.Eventually(t, func() bool { return kv.written(globalInvalidationKey) }, time.Second, time.Millisecond)
		assert.Equal(t, int64(2), attempts.Load())
	})

	t.Run("stops after the configured attempts", func(t *testing.T) {
		kv := newFlakyKV()
		var attempts atomic.Int64
		kv.failPut = func(key string) error {
			if key == globalInvalidationKey {
				attempts.Add(1)
				return errors.New("kv down")
			}
			return nil
		}
		CacheLayer{bucket: kv}.followUpFlush(context.Background(), time.Millisecond)
		assert.Eventually(t, func() bool { return attempts.Load() == int64(modelAnnounceAttempts) }, time.Second, time.Millisecond)
		time.Sleep(30 * time.Millisecond)
		assert.Equal(t, int64(modelAnnounceAttempts), attempts.Load(), "no further attempts")
		assert.False(t, kv.written(globalInvalidationKey))
	})
}
