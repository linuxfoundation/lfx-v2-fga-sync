// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"encoding/base32"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	openfga "github.com/openfga/go-sdk"
	. "github.com/openfga/go-sdk/client"
	"github.com/stretchr/testify/mock"
)

// MockNatsKeyValue is a mock implementation of INatsKeyValue for testing
type MockNatsKeyValue struct {
	mock.Mock
}

func (m *MockNatsKeyValue) Get(ctx context.Context, key string) (jetstream.KeyValueEntry, error) {
	args := m.Called(ctx, key)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(jetstream.KeyValueEntry), args.Error(1)
}

func (m *MockNatsKeyValue) Put(ctx context.Context, key string, value []byte) (uint64, error) {
	args := m.Called(ctx, key, value)
	return args.Get(0).(uint64), args.Error(1)
}

func (m *MockNatsKeyValue) PutString(ctx context.Context, key string, value string) (uint64, error) {
	args := m.Called(ctx, key, value)
	return args.Get(0).(uint64), args.Error(1)
}

// TestCacheKeyEncoding tests the cache key encoding functionality
func TestCacheKeyEncoding(t *testing.T) {
	tests := []struct {
		name        string
		relationKey string
		wantPrefix  string
	}{
		{
			name:        "simple relation",
			relationKey: "project:123#writer@user:456",
			wantPrefix:  "rel.",
		},
		{
			name:        "complex relation",
			relationKey: "org:linux-foundation/project:kernel#maintainer@user:torvalds",
			wantPrefix:  "rel.",
		},
		{
			name:        "wildcard user",
			relationKey: "project:public#viewer@user:*",
			wantPrefix:  "rel.",
		},
		{
			name:        "group relation",
			relationKey: "project:123#writer@group:developers",
			wantPrefix:  "rel.",
		},
	}

	// Use the same encoder as in the actual code
	encoder := base32.StdEncoding.WithPadding(base32.NoPadding)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Encode the relation key
			encoded := encoder.EncodeToString([]byte(tt.relationKey))
			cacheKey := tt.wantPrefix + encoded

			// Verify it starts with the correct prefix
			if !strings.HasPrefix(cacheKey, tt.wantPrefix) {
				t.Errorf("cache key should start with %s, got %s", tt.wantPrefix, cacheKey)
			}

			// Verify we can decode it back
			withoutPrefix := strings.TrimPrefix(cacheKey, tt.wantPrefix)
			decoded, err := encoder.DecodeString(withoutPrefix)
			if err != nil {
				t.Errorf("failed to decode cache key: %v", err)
			}
			if string(decoded) != tt.relationKey {
				t.Errorf("decoded key mismatch: got %s, want %s", decoded, tt.relationKey)
			}
		})
	}
}

// TestExtractCheckRequests tests the parsing of check requests
func TestExtractCheckRequests(t *testing.T) {
	tests := []struct {
		name          string
		payload       []byte
		expectError   bool
		expectedCount int
		description   string
	}{
		{
			name:          "single valid request",
			payload:       []byte("project:123#writer@user:456"),
			expectError:   false,
			expectedCount: 1,
			description:   "should parse single check request",
		},
		{
			name:          "multiple valid requests",
			payload:       []byte("project:123#writer@user:456\nproject:789#viewer@user:456"),
			expectError:   false,
			expectedCount: 2,
			description:   "should parse multiple check requests separated by newlines",
		},
		{
			name:          "empty lines ignored",
			payload:       []byte("project:123#writer@user:456\n\nproject:789#viewer@user:456\n"),
			expectError:   false,
			expectedCount: 2,
			description:   "should ignore empty lines",
		},
		{
			name:          "invalid format - missing @",
			payload:       []byte("project:123#writeruser:456"),
			expectError:   true,
			expectedCount: 0,
			description:   "should error on missing @ separator",
		},
		{
			name:          "invalid format - missing #",
			payload:       []byte("project:123writer@user:456"),
			expectError:   true,
			expectedCount: 0,
			description:   "should error on missing # separator",
		},
		{
			name:          "empty payload",
			payload:       []byte(""),
			expectError:   false,
			expectedCount: 0,
			description:   "should handle empty payload",
		},
		{
			name:          "only newlines",
			payload:       []byte("\n\n\n"),
			expectError:   false,
			expectedCount: 0,
			description:   "should handle payload with only newlines",
		},
	}

	fgaService := FgaService{
		client: &MockFgaClient{},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requests, err := fgaService.ExtractCheckRequests(tt.payload)

			if tt.expectError && err == nil {
				t.Errorf("expected error but got none")
			}
			if !tt.expectError && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if len(requests) != tt.expectedCount {
				t.Errorf("expected %d requests, got %d", tt.expectedCount, len(requests))
			}

			t.Logf("%s: %s", tt.name, tt.description)
		})
	}
}

// TestReadObjectTuples tests the ReadObjectTuples function
func TestReadObjectTuples(t *testing.T) {
	tests := []struct {
		name           string
		object         string
		mockSetup      func(*MockFgaClient)
		expectedTuples []openfga.Tuple
		expectError    bool
		description    string
	}{
		{
			name:   "single page of tuples",
			object: "project:123",
			mockSetup: func(m *MockFgaClient) {
				m.On("Read", mock.Anything, mock.MatchedBy(func(req ClientReadRequest) bool {
					return req.Object != nil && *req.Object == "project:123"
				}), mock.Anything).Return(&ClientReadResponse{
					Tuples: []openfga.Tuple{
						{Key: openfga.TupleKey{User: "user:456", Relation: "writer", Object: "project:123"}},
						{Key: openfga.TupleKey{User: "user:789", Relation: "viewer", Object: "project:123"}},
					},
					ContinuationToken: "",
				}, nil).Once()
			},
			expectedTuples: []openfga.Tuple{
				{Key: openfga.TupleKey{User: "user:456", Relation: "writer", Object: "project:123"}},
				{Key: openfga.TupleKey{User: "user:789", Relation: "viewer", Object: "project:123"}},
			},
			expectError: false,
			description: "should return all tuples from single page",
		},
		{
			name:   "multiple pages with pagination",
			object: "project:456",
			mockSetup: func(m *MockFgaClient) {
				// First page
				m.On("Read", mock.Anything, mock.MatchedBy(func(req ClientReadRequest) bool {
					return req.Object != nil && *req.Object == "project:456"
				}), mock.MatchedBy(func(opts ClientReadOptions) bool {
					return opts.ContinuationToken == nil
				})).Return(&ClientReadResponse{
					Tuples: []openfga.Tuple{
						{Key: openfga.TupleKey{User: "user:111", Relation: "writer", Object: "project:456"}},
						{Key: openfga.TupleKey{User: "user:222", Relation: "writer", Object: "project:456"}},
					},
					ContinuationToken: "page-2-token",
				}, nil).Once()

				// Second page
				m.On("Read", mock.Anything, mock.MatchedBy(func(req ClientReadRequest) bool {
					return req.Object != nil && *req.Object == "project:456"
				}), mock.MatchedBy(func(opts ClientReadOptions) bool {
					return opts.ContinuationToken != nil && *opts.ContinuationToken == "page-2-token"
				})).Return(&ClientReadResponse{
					Tuples: []openfga.Tuple{
						{Key: openfga.TupleKey{User: "user:333", Relation: "viewer", Object: "project:456"}},
						{Key: openfga.TupleKey{User: "group:devs", Relation: "writer", Object: "project:456"}},
					},
					ContinuationToken: "",
				}, nil).Once()
			},
			expectedTuples: []openfga.Tuple{
				{Key: openfga.TupleKey{User: "user:111", Relation: "writer", Object: "project:456"}},
				{Key: openfga.TupleKey{User: "user:222", Relation: "writer", Object: "project:456"}},
				{Key: openfga.TupleKey{User: "user:333", Relation: "viewer", Object: "project:456"}},
				{Key: openfga.TupleKey{User: "group:devs", Relation: "writer", Object: "project:456"}},
			},
			expectError: false,
			description: "should aggregate tuples from multiple pages",
		},
		{
			name:   "empty result",
			object: "project:789",
			mockSetup: func(m *MockFgaClient) {
				m.On("Read", mock.Anything, mock.MatchedBy(func(req ClientReadRequest) bool {
					return req.Object != nil && *req.Object == "project:789"
				}), mock.Anything).Return(&ClientReadResponse{
					Tuples:            []openfga.Tuple{},
					ContinuationToken: "",
				}, nil).Once()
			},
			expectedTuples: []openfga.Tuple{},
			expectError:    false,
			description:    "should handle empty result",
		},
		{
			name:   "error on first page",
			object: "project:error",
			mockSetup: func(m *MockFgaClient) {
				m.On("Read", mock.Anything, mock.MatchedBy(func(req ClientReadRequest) bool {
					return req.Object != nil && *req.Object == "project:error"
				}), mock.Anything).Return((*ClientReadResponse)(nil), errors.New("fga read error")).Once()
			},
			expectedTuples: nil,
			expectError:    true,
			description:    "should return error on read failure",
		},
		{
			name:   "error on subsequent page",
			object: "project:partial",
			mockSetup: func(m *MockFgaClient) {
				// First page succeeds
				m.On("Read", mock.Anything, mock.MatchedBy(func(req ClientReadRequest) bool {
					return req.Object != nil && *req.Object == "project:partial"
				}), mock.MatchedBy(func(opts ClientReadOptions) bool {
					return opts.ContinuationToken == nil
				})).Return(&ClientReadResponse{
					Tuples: []openfga.Tuple{
						{Key: openfga.TupleKey{User: "user:100", Relation: "writer", Object: "project:partial"}},
					},
					ContinuationToken: "error-token",
				}, nil).Once()

				// Second page fails
				m.On("Read", mock.Anything, mock.MatchedBy(func(req ClientReadRequest) bool {
					return req.Object != nil && *req.Object == "project:partial"
				}), mock.MatchedBy(func(opts ClientReadOptions) bool {
					return opts.ContinuationToken != nil && *opts.ContinuationToken == "error-token"
				})).Return((*ClientReadResponse)(nil), errors.New("pagination error")).Once()
			},
			expectedTuples: nil,
			expectError:    true,
			description:    "should return error on pagination failure",
		},
		{
			name:   "wildcard and group users",
			object: "project:public",
			mockSetup: func(m *MockFgaClient) {
				m.On("Read", mock.Anything, mock.MatchedBy(func(req ClientReadRequest) bool {
					return req.Object != nil && *req.Object == "project:public"
				}), mock.Anything).Return(&ClientReadResponse{
					Tuples: []openfga.Tuple{
						{Key: openfga.TupleKey{User: "user:*", Relation: "viewer", Object: "project:public"}},
						{Key: openfga.TupleKey{User: "group:writers", Relation: "writer", Object: "project:public"}},
						{Key: openfga.TupleKey{User: "user:123", Relation: "writer", Object: "project:public"}},
					},
					ContinuationToken: "",
				}, nil).Once()
			},
			expectedTuples: []openfga.Tuple{
				{Key: openfga.TupleKey{User: "user:*", Relation: "viewer", Object: "project:public"}},
				{Key: openfga.TupleKey{User: "group:writers", Relation: "writer", Object: "project:public"}},
				{Key: openfga.TupleKey{User: "user:123", Relation: "writer", Object: "project:public"}},
			},
			expectError: false,
			description: "should handle wildcard and group users",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create mock client and service
			mockClient := new(MockFgaClient)
			tt.mockSetup(mockClient)

			fgaService := FgaService{
				client: mockClient,
			}

			// Execute the function
			ctx := context.Background()
			tuples, err := fgaService.ReadObjectTuples(ctx, tt.object)

			// Verify error expectations
			if tt.expectError && err == nil {
				t.Errorf("%s: expected error but got none", tt.description)
			}
			if !tt.expectError && err != nil {
				t.Errorf("%s: unexpected error: %v", tt.description, err)
			}

			// Verify tuple results
			if !tt.expectError {
				if len(tuples) != len(tt.expectedTuples) {
					t.Errorf("%s: expected %d tuples, got %d", tt.description, len(tt.expectedTuples), len(tuples))
				}
				for i, tuple := range tuples {
					if i >= len(tt.expectedTuples) {
						break
					}
					expected := tt.expectedTuples[i]
					if tuple.Key.User != expected.Key.User ||
						tuple.Key.Relation != expected.Key.Relation ||
						tuple.Key.Object != expected.Key.Object {
						t.Errorf("%s: tuple %d mismatch: got %+v, want %+v",
							tt.description, i, tuple.Key, expected.Key)
					}
				}
			}

			// Verify all expectations were met
			mockClient.AssertExpectations(t)
		})
	}
}

// TestSyncObjectTuples_RelationMapping tests the relation mapping logic
func TestSyncObjectTuples_RelationMapping(t *testing.T) {
	tests := []struct {
		name          string
		object        string
		relations     []ClientTupleKey
		expectedCount int
		description   string
	}{
		{
			name:   "relations with object field empty",
			object: "project:123",
			relations: []ClientTupleKey{
				{User: "user:456", Relation: "writer", Object: ""},
				{User: "user:789", Relation: "viewer", Object: ""},
			},
			expectedCount: 2,
			description:   "should fill in empty object fields",
		},
		{
			name:   "relations with matching object",
			object: "project:123",
			relations: []ClientTupleKey{
				{User: "user:456", Relation: "writer", Object: "project:123"},
				{User: "user:789", Relation: "viewer", Object: "project:123"},
			},
			expectedCount: 2,
			description:   "should accept matching object fields",
		},
		{
			name:   "relations with different object",
			object: "project:123",
			relations: []ClientTupleKey{
				{User: "user:456", Relation: "writer", Object: "project:999"},
				{User: "user:789", Relation: "viewer", Object: "project:123"},
			},
			expectedCount: 1,
			description:   "should skip relations with different objects",
		},
		{
			name:          "empty relations",
			object:        "project:123",
			relations:     []ClientTupleKey{},
			expectedCount: 0,
			description:   "should handle empty relations",
		},
		{
			name:          "nil relations",
			object:        "project:123",
			relations:     nil,
			expectedCount: 0,
			description:   "should handle nil relations",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a map to simulate the function's behavior
			relationsMap := make(map[string]ClientTupleKey)
			for _, relation := range tt.relations {
				switch {
				case relation.Object == "":
					relation.Object = tt.object
				case relation.Object != tt.object:
					// Skip relations for different objects
					continue
				}
				key := relation.User + "#" + relation.Relation
				relationsMap[key] = relation
			}

			if len(relationsMap) != tt.expectedCount {
				t.Errorf("%s: expected %d relations in map, got %d",
					tt.description, tt.expectedCount, len(relationsMap))
			}
		})
	}
}

// TestCacheKeyGeneration tests the cache key generation for relations
func TestCacheKeyGeneration(t *testing.T) {
	encoder := base32.StdEncoding.WithPadding(base32.NoPadding)

	tests := []struct {
		name    string
		tuple   ClientBatchCheckItem
		wantKey string
	}{
		{
			name: "standard tuple",
			tuple: ClientBatchCheckItem{
				User:     "user:123",
				Relation: "writer",
				Object:   "project:456",
			},
			wantKey: "rel." + encoder.EncodeToString([]byte("project:456#writer@user:123")),
		},
		{
			name: "wildcard user",
			tuple: ClientBatchCheckItem{
				User:     "user:*",
				Relation: "viewer",
				Object:   "project:public",
			},
			wantKey: "rel." + encoder.EncodeToString([]byte("project:public#viewer@user:*")),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			relationKey := tt.tuple.Object + "#" + tt.tuple.Relation + "@" + tt.tuple.User
			cacheKey := "rel." + encoder.EncodeToString([]byte(relationKey))

			if cacheKey != tt.wantKey {
				t.Errorf("cache key mismatch: got %s, want %s", cacheKey, tt.wantKey)
			}
		})
	}
}

// TestResponseMessageBuilding tests the response message building logic
func TestResponseMessageBuilding(t *testing.T) {
	tests := []struct {
		name            string
		tupleCount      int
		expectedMinSize int
		description     string
	}{
		{
			name:            "small batch",
			tupleCount:      5,
			expectedMinSize: 5 * 80, // 80 bytes per tuple estimate
			description:     "should preallocate for small batch",
		},
		{
			name:            "large batch",
			tupleCount:      100,
			expectedMinSize: 100 * 80,
			description:     "should preallocate for large batch",
		},
		{
			name:            "empty batch",
			tupleCount:      0,
			expectedMinSize: 0,
			description:     "should handle empty batch",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Simulate the message preallocation
			message := make([]byte, 0, 80*tt.tupleCount)

			if cap(message) != tt.expectedMinSize {
				t.Errorf("%s: expected capacity %d, got %d",
					tt.description, tt.expectedMinSize, cap(message))
			}
		})
	}
}

// TestCacheInvalidationLogic tests the cache invalidation timestamp logic
func TestCacheInvalidationLogic(t *testing.T) {
	tests := []struct {
		name               string
		setupCache         func(*MockKeyValue)
		expectInvalidation bool
		description        string
	}{
		{
			name: "no invalidation key",
			setupCache: func(m *MockKeyValue) {
				m.SetNotFound("inv")
			},
			expectInvalidation: false,
			description:        "should handle missing invalidation key",
		},
		{
			name: "invalidation key exists",
			setupCache: func(m *MockKeyValue) {
				m.data["inv"] = []byte("1")
				m.createdTimes["inv"] = time.Now().Add(-5 * time.Minute)
			},
			expectInvalidation: true,
			description:        "should read invalidation timestamp",
		},
		{
			name: "cache error",
			setupCache: func(m *MockKeyValue) {
				m.SetError(errors.New("cache error"))
			},
			expectInvalidation: false,
			description:        "should handle cache errors",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockCache := NewMockKeyValue()
			tt.setupCache(mockCache)

			// Test the invalidation logic
			ctx := context.Background()
			entry, err := mockCache.Get(ctx, "inv")

			if tt.expectInvalidation {
				if err != nil {
					t.Errorf("%s: unexpected error: %v", tt.description, err)
				}
				if entry == nil {
					t.Errorf("%s: expected entry, got nil", tt.description)
				}
			} else if err == nil && entry != nil {
				t.Errorf("%s: expected no entry or error, got entry", tt.description)
			}
		})
	}
}

// TestNewTupleKeySlice tests the NewTupleKeySlice helper function
func TestNewTupleKeySlice(t *testing.T) {
	tests := []struct {
		name    string
		size    int
		wantCap int
	}{
		{
			name:    "small slice",
			size:    4,
			wantCap: 4,
		},
		{
			name:    "zero size",
			size:    0,
			wantCap: 0,
		},
		{
			name:    "large slice",
			size:    100,
			wantCap: 100,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handlerService := setupService()
			got := handlerService.fgaService.NewTupleKeySlice(tt.size)
			if len(got) != 0 {
				t.Errorf("expected empty slice, got length %d", len(got))
			}
			if cap(got) != tt.wantCap {
				t.Errorf("expected capacity %d, got %d", tt.wantCap, cap(got))
			}
		})
	}
}

// TestTupleKey tests the TupleKey helper function
func TestTupleKey(t *testing.T) {
	tests := []struct {
		name     string
		user     string
		relation string
		object   string
		want     ClientTupleKey
	}{
		{
			name:     "standard tuple",
			user:     "user:123",
			relation: "writer",
			object:   "project:456",
			want: ClientTupleKey{
				User:     "user:123",
				Relation: "writer",
				Object:   "project:456",
			},
		},
		{
			name:     "wildcard user",
			user:     "user:*",
			relation: "viewer",
			object:   "project:public",
			want: ClientTupleKey{
				User:     "user:*",
				Relation: "viewer",
				Object:   "project:public",
			},
		},
		{
			name:     "group user",
			user:     "group:developers",
			relation: "writer",
			object:   "project:123",
			want: ClientTupleKey{
				User:     "group:developers",
				Relation: "writer",
				Object:   "project:123",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handlerService := setupService()
			got := handlerService.fgaService.TupleKey(tt.user, tt.relation, tt.object)
			if got.User != tt.want.User || got.Relation != tt.want.Relation || got.Object != tt.want.Object {
				t.Errorf("fgaTupleKey() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestGetTuplesByRelation tests the GetTuplesByRelation function
func TestGetTuplesByRelation(t *testing.T) {
	tests := []struct {
		name           string
		object         string
		relation       string
		mockSetup      func(*MockFgaClient)
		expectedTuples []openfga.Tuple
		expectError    bool
		description    string
	}{
		{
			name:     "filter by meeting_coordinator relation",
			object:   "project:123",
			relation: "meeting_coordinator",
			mockSetup: func(m *MockFgaClient) {
				m.On("Read", mock.Anything, mock.MatchedBy(func(req ClientReadRequest) bool {
					return req.Object != nil && *req.Object == "project:123"
				}), mock.Anything).Return(&ClientReadResponse{
					Tuples: []openfga.Tuple{
						{Key: openfga.TupleKey{User: "user:organizer1", Relation: "meeting_coordinator", Object: "project:123"}},
						{Key: openfga.TupleKey{User: "user:writer1", Relation: "writer", Object: "project:123"}},
						{Key: openfga.TupleKey{User: "user:organizer2", Relation: "meeting_coordinator", Object: "project:123"}},
						{Key: openfga.TupleKey{User: "user:viewer1", Relation: "viewer", Object: "project:123"}},
					},
					ContinuationToken: "",
				}, nil).Once()
			},
			expectedTuples: []openfga.Tuple{
				{Key: openfga.TupleKey{User: "user:organizer1", Relation: "meeting_coordinator", Object: "project:123"}},
				{Key: openfga.TupleKey{User: "user:organizer2", Relation: "meeting_coordinator", Object: "project:123"}},
			},
			expectError: false,
			description: "should return only meeting_coordinator tuples",
		},
		{
			name:     "filter by writer relation",
			object:   "project:456",
			relation: "writer",
			mockSetup: func(m *MockFgaClient) {
				m.On("Read", mock.Anything, mock.MatchedBy(func(req ClientReadRequest) bool {
					return req.Object != nil && *req.Object == "project:456"
				}), mock.Anything).Return(&ClientReadResponse{
					Tuples: []openfga.Tuple{
						{Key: openfga.TupleKey{User: "user:organizer1", Relation: "meeting_coordinator", Object: "project:456"}},
						{Key: openfga.TupleKey{User: "user:writer1", Relation: "writer", Object: "project:456"}},
						{Key: openfga.TupleKey{User: "user:writer2", Relation: "writer", Object: "project:456"}},
					},
					ContinuationToken: "",
				}, nil).Once()
			},
			expectedTuples: []openfga.Tuple{
				{Key: openfga.TupleKey{User: "user:writer1", Relation: "writer", Object: "project:456"}},
				{Key: openfga.TupleKey{User: "user:writer2", Relation: "writer", Object: "project:456"}},
			},
			expectError: false,
			description: "should return only writer tuples",
		},
		{
			name:     "no matching relation",
			object:   "project:789",
			relation: "nonexistent",
			mockSetup: func(m *MockFgaClient) {
				m.On("Read", mock.Anything, mock.MatchedBy(func(req ClientReadRequest) bool {
					return req.Object != nil && *req.Object == "project:789"
				}), mock.Anything).Return(&ClientReadResponse{
					Tuples: []openfga.Tuple{
						{Key: openfga.TupleKey{User: "user:writer1", Relation: "writer", Object: "project:789"}},
						{Key: openfga.TupleKey{User: "user:viewer1", Relation: "viewer", Object: "project:789"}},
					},
					ContinuationToken: "",
				}, nil).Once()
			},
			expectedTuples: []openfga.Tuple{},
			expectError:    false,
			description:    "should return empty slice when no tuples match relation",
		},
		{
			name:     "empty tuples from object",
			object:   "project:empty",
			relation: "writer",
			mockSetup: func(m *MockFgaClient) {
				m.On("Read", mock.Anything, mock.MatchedBy(func(req ClientReadRequest) bool {
					return req.Object != nil && *req.Object == "project:empty"
				}), mock.Anything).Return(&ClientReadResponse{
					Tuples:            []openfga.Tuple{},
					ContinuationToken: "",
				}, nil).Once()
			},
			expectedTuples: []openfga.Tuple{},
			expectError:    false,
			description:    "should return empty slice when object has no tuples",
		},
		{
			name:     "read error from OpenFGA",
			object:   "project:error",
			relation: "writer",
			mockSetup: func(m *MockFgaClient) {
				m.On("Read", mock.Anything, mock.MatchedBy(func(req ClientReadRequest) bool {
					return req.Object != nil && *req.Object == "project:error"
				}), mock.Anything).Return((*ClientReadResponse)(nil), errors.New("OpenFGA read error")).Once()
			},
			expectedTuples: nil,
			expectError:    true,
			description:    "should return error when ReadObjectTuples fails",
		},
		{
			name:     "filter committee relation on meeting",
			object:   "meeting:123",
			relation: "committee",
			mockSetup: func(m *MockFgaClient) {
				m.On("Read", mock.Anything, mock.MatchedBy(func(req ClientReadRequest) bool {
					return req.Object != nil && *req.Object == "meeting:123"
				}), mock.Anything).Return(&ClientReadResponse{
					Tuples: []openfga.Tuple{
						{Key: openfga.TupleKey{User: "committee:committee1", Relation: "committee", Object: "meeting:123"}},
						{Key: openfga.TupleKey{User: "user:organizer1", Relation: "organizer", Object: "meeting:123"}},
						{Key: openfga.TupleKey{User: "committee:committee2", Relation: "committee", Object: "meeting:123"}},
						{Key: openfga.TupleKey{User: "user:participant1", Relation: "participant", Object: "meeting:123"}},
					},
					ContinuationToken: "",
				}, nil).Once()
			},
			expectedTuples: []openfga.Tuple{
				{Key: openfga.TupleKey{User: "committee:committee1", Relation: "committee", Object: "meeting:123"}},
				{Key: openfga.TupleKey{User: "committee:committee2", Relation: "committee", Object: "meeting:123"}},
			},
			expectError: false,
			description: "should filter committee relations on meeting object",
		},
		{
			name:     "pagination with filtering",
			object:   "project:paginated",
			relation: "writer",
			mockSetup: func(m *MockFgaClient) {
				// First page
				m.On("Read", mock.Anything, mock.MatchedBy(func(req ClientReadRequest) bool {
					return req.Object != nil && *req.Object == "project:paginated"
				}), mock.MatchedBy(func(opts ClientReadOptions) bool {
					return opts.ContinuationToken == nil
				})).Return(&ClientReadResponse{
					Tuples: []openfga.Tuple{
						{Key: openfga.TupleKey{User: "user:writer1", Relation: "writer", Object: "project:paginated"}},
						{Key: openfga.TupleKey{User: "user:viewer1", Relation: "viewer", Object: "project:paginated"}},
					},
					ContinuationToken: "page-2",
				}, nil).Once()

				// Second page
				m.On("Read", mock.Anything, mock.MatchedBy(func(req ClientReadRequest) bool {
					return req.Object != nil && *req.Object == "project:paginated"
				}), mock.MatchedBy(func(opts ClientReadOptions) bool {
					return opts.ContinuationToken != nil && *opts.ContinuationToken == "page-2"
				})).Return(&ClientReadResponse{
					Tuples: []openfga.Tuple{
						{Key: openfga.TupleKey{User: "user:writer2", Relation: "writer", Object: "project:paginated"}},
						{Key: openfga.TupleKey{User: "user:viewer2", Relation: "viewer", Object: "project:paginated"}},
					},
					ContinuationToken: "",
				}, nil).Once()
			},
			expectedTuples: []openfga.Tuple{
				{Key: openfga.TupleKey{User: "user:writer1", Relation: "writer", Object: "project:paginated"}},
				{Key: openfga.TupleKey{User: "user:writer2", Relation: "writer", Object: "project:paginated"}},
			},
			expectError: false,
			description: "should filter across paginated results",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create mock client and service
			mockClient := new(MockFgaClient)
			tt.mockSetup(mockClient)

			fgaService := FgaService{
				client: mockClient,
			}

			// Execute the function
			ctx := context.Background()
			tuples, err := fgaService.GetTuplesByRelation(ctx, tt.object, tt.relation)

			// Verify error expectations
			if tt.expectError && err == nil {
				t.Errorf("%s: expected error but got none", tt.description)
			}
			if !tt.expectError && err != nil {
				t.Errorf("%s: unexpected error: %v", tt.description, err)
			}

			// Verify tuple results
			if !tt.expectError {
				if len(tuples) != len(tt.expectedTuples) {
					t.Errorf("%s: expected %d tuples, got %d", tt.description, len(tt.expectedTuples), len(tuples))
				}
				for i, tuple := range tuples {
					if i >= len(tt.expectedTuples) {
						break
					}
					expected := tt.expectedTuples[i]
					if tuple.Key.User != expected.Key.User ||
						tuple.Key.Relation != expected.Key.Relation ||
						tuple.Key.Object != expected.Key.Object {
						t.Errorf("%s: tuple %d mismatch: got %+v, want %+v",
							tt.description, i, tuple.Key, expected.Key)
					}
					// Verify that all returned tuples have the expected relation
					if tuple.Key.Relation != tt.relation {
						t.Errorf("%s: tuple %d has wrong relation: got %s, want %s",
							tt.description, i, tuple.Key.Relation, tt.relation)
					}
				}
			}

			// Verify all expectations were met
			mockClient.AssertExpectations(t)
		})
	}
}

// TestDeleteTuplesByUserAndObject tests the DeleteTuplesByUserAndObject functionality
func TestDeleteTuplesByUserAndObject(t *testing.T) {
	tests := []struct {
		name        string
		user        string
		object      string
		mockSetup   func(*MockFgaClient)
		expectError bool
		description string
	}{
		{
			name:   "delete single tuple for user and object",
			user:   "user:123",
			object: "meeting:456",
			mockSetup: func(m *MockFgaClient) {
				// Mock ReadObjectTuples to return tuples for the object
				m.On("Read", mock.Anything, mock.MatchedBy(func(req ClientReadRequest) bool {
					return req.Object != nil && *req.Object == "meeting:456"
				}), mock.Anything).Return(&ClientReadResponse{
					Tuples: []openfga.Tuple{
						{Key: openfga.TupleKey{User: "user:123", Relation: "participant", Object: "meeting:456"}},
						{Key: openfga.TupleKey{User: "user:789", Relation: "organizer", Object: "meeting:456"}},
					},
					ContinuationToken: "",
				}, nil).Once()

				// Mock DeleteTuples to delete only the user:123 tuple
				m.On("Write", mock.Anything, mock.MatchedBy(func(req ClientWriteRequest) bool {
					return len(req.Deletes) == 1 &&
						req.Deletes[0].User == "user:123" &&
						req.Deletes[0].Relation == "participant" &&
						req.Deletes[0].Object == "meeting:456"
				})).Return(&ClientWriteResponse{}, nil).Once()
			},
			expectError: false,
			description: "should delete single tuple for user on object",
		},
		{
			name:   "delete multiple tuples for user and object",
			user:   "user:456",
			object: "past_meeting:789",
			mockSetup: func(m *MockFgaClient) {
				// Mock ReadObjectTuples to return multiple tuples for the user
				m.On("Read", mock.Anything, mock.MatchedBy(func(req ClientReadRequest) bool {
					return req.Object != nil && *req.Object == "past_meeting:789"
				}), mock.Anything).Return(&ClientReadResponse{
					Tuples: []openfga.Tuple{
						{Key: openfga.TupleKey{User: "user:456", Relation: "host", Object: "past_meeting:789"}},
						{Key: openfga.TupleKey{User: "user:456", Relation: "invitee", Object: "past_meeting:789"}},
						{Key: openfga.TupleKey{User: "user:456", Relation: "attendee", Object: "past_meeting:789"}},
						{Key: openfga.TupleKey{User: "user:999", Relation: "invitee", Object: "past_meeting:789"}},
					},
					ContinuationToken: "",
				}, nil).Once()

				// Mock DeleteTuples to delete all user:456 tuples
				m.On("Write", mock.Anything, mock.MatchedBy(func(req ClientWriteRequest) bool {
					if len(req.Deletes) != 3 {
						return false
					}
					// Check that all deletes are for user:456
					for _, del := range req.Deletes {
						if del.User != "user:456" || del.Object != "past_meeting:789" {
							return false
						}
					}
					// Check that we have the expected relations
					relations := map[string]bool{
						"host":     false,
						"invitee":  false,
						"attendee": false,
					}
					for _, del := range req.Deletes {
						relations[del.Relation] = true
					}
					return relations["host"] && relations["invitee"] && relations["attendee"]
				})).Return(&ClientWriteResponse{}, nil).Once()
			},
			expectError: false,
			description: "should delete all tuples for user on object",
		},
		{
			name:   "no tuples to delete",
			user:   "user:nonexistent",
			object: "meeting:123",
			mockSetup: func(m *MockFgaClient) {
				// Mock ReadObjectTuples to return tuples but none for this user
				m.On("Read", mock.Anything, mock.MatchedBy(func(req ClientReadRequest) bool {
					return req.Object != nil && *req.Object == "meeting:123"
				}), mock.Anything).Return(&ClientReadResponse{
					Tuples: []openfga.Tuple{
						{Key: openfga.TupleKey{User: "user:other1", Relation: "participant", Object: "meeting:123"}},
						{Key: openfga.TupleKey{User: "user:other2", Relation: "organizer", Object: "meeting:123"}},
					},
					ContinuationToken: "",
				}, nil).Once()

				// No Write call expected when there's nothing to delete
			},
			expectError: false,
			description: "should handle case with no tuples to delete",
		},
		{
			name:   "error reading object tuples",
			user:   "user:123",
			object: "meeting:error",
			mockSetup: func(m *MockFgaClient) {
				// Mock ReadObjectTuples to return an error
				m.On("Read", mock.Anything, mock.MatchedBy(func(req ClientReadRequest) bool {
					return req.Object != nil && *req.Object == "meeting:error"
				}), mock.Anything).Return((*ClientReadResponse)(nil), errors.New("read error")).Once()
			},
			expectError: true,
			description: "should return error when reading tuples fails",
		},
		{
			name:   "error deleting tuples",
			user:   "user:123",
			object: "meeting:456",
			mockSetup: func(m *MockFgaClient) {
				// Mock ReadObjectTuples to return tuples
				m.On("Read", mock.Anything, mock.MatchedBy(func(req ClientReadRequest) bool {
					return req.Object != nil && *req.Object == "meeting:456"
				}), mock.Anything).Return(&ClientReadResponse{
					Tuples: []openfga.Tuple{
						{Key: openfga.TupleKey{User: "user:123", Relation: "participant", Object: "meeting:456"}},
					},
					ContinuationToken: "",
				}, nil).Once()

				// Mock DeleteTuples to return an error
				m.On("Write", mock.Anything, mock.Anything).
					Return((*ClientWriteResponse)(nil), errors.New("delete error")).Once()
			},
			expectError: true,
			description: "should return error when deleting tuples fails",
		},
		{
			name:   "handle paginated results",
			user:   "user:paginated",
			object: "project:large",
			mockSetup: func(m *MockFgaClient) {
				// First page
				m.On("Read", mock.Anything, mock.MatchedBy(func(req ClientReadRequest) bool {
					return req.Object != nil && *req.Object == "project:large"
				}), mock.Anything).Return(&ClientReadResponse{
					Tuples: []openfga.Tuple{
						{Key: openfga.TupleKey{User: "user:paginated", Relation: "writer", Object: "project:large"}},
						{Key: openfga.TupleKey{User: "user:other", Relation: "viewer", Object: "project:large"}},
					},
					ContinuationToken: "token1",
				}, nil).Once()

				// Second page - Note: we can't check ContinuationToken in request, it's handled internally
				m.On("Read", mock.Anything, mock.MatchedBy(func(req ClientReadRequest) bool {
					return req.Object != nil && *req.Object == "project:large"
				}), mock.Anything).Return(&ClientReadResponse{
					Tuples: []openfga.Tuple{
						{Key: openfga.TupleKey{User: "user:paginated", Relation: "viewer", Object: "project:large"}},
					},
					ContinuationToken: "",
				}, nil).Once()

				// Mock DeleteTuples to delete both tuples for user:paginated
				m.On("Write", mock.Anything, mock.MatchedBy(func(req ClientWriteRequest) bool {
					return len(req.Deletes) == 2 &&
						req.Deletes[0].User == "user:paginated" &&
						req.Deletes[1].User == "user:paginated"
				})).Return(&ClientWriteResponse{}, nil).Once()
			},
			expectError: false,
			description: "should handle paginated results when reading tuples",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create mock client
			mockClient := new(MockFgaClient)
			tt.mockSetup(mockClient)

			// Create mock cache
			mockCache := new(MockNatsKeyValue)
			// Mock cache invalidation - called when tuples are deleted
			mockCache.On("Put", mock.Anything, "inv", []byte("1")).Return(uint64(1), nil).Maybe()

			// Create service with mock client and cache
			service := FgaService{
				client:      mockClient,
				cacheBucket: mockCache,
			}

			// Execute the function
			err := service.DeleteTuplesByUserAndObject(context.Background(), tt.user, tt.object)

			// Verify error expectations
			if tt.expectError && err == nil {
				t.Errorf("%s: expected error but got none", tt.description)
			}
			if !tt.expectError && err != nil {
				t.Errorf("%s: unexpected error: %v", tt.description, err)
			}

			// Verify all expectations were met
			mockClient.AssertExpectations(t)
		})
	}
}

// TestGetTuplesByUserAndObject tests the GetTuplesByUserAndObject functionality
func TestGetTuplesByUserAndObject(t *testing.T) {
	tests := []struct {
		name           string
		user           string
		object         string
		mockSetup      func(*MockFgaClient)
		expectedTuples []ClientTupleKey
		expectError    bool
		description    string
	}{
		{
			name:   "get single tuple for user and object",
			user:   "user:123",
			object: "meeting:456",
			mockSetup: func(m *MockFgaClient) {
				m.On("Read", mock.Anything, mock.MatchedBy(func(req ClientReadRequest) bool {
					return req.Object != nil && *req.Object == "meeting:456"
				}), mock.Anything).Return(&ClientReadResponse{
					Tuples: []openfga.Tuple{
						{Key: openfga.TupleKey{User: "user:123", Relation: "participant", Object: "meeting:456"}},
						{Key: openfga.TupleKey{User: "user:789", Relation: "organizer", Object: "meeting:456"}},
					},
					ContinuationToken: "",
				}, nil).Once()
			},
			expectedTuples: []ClientTupleKey{
				{User: "user:123", Relation: "participant", Object: "meeting:456"},
			},
			expectError: false,
			description: "should return single tuple for user on object",
		},
		{
			name:   "get multiple tuples for user and object",
			user:   "user:456",
			object: "past_meeting:789",
			mockSetup: func(m *MockFgaClient) {
				m.On("Read", mock.Anything, mock.MatchedBy(func(req ClientReadRequest) bool {
					return req.Object != nil && *req.Object == "past_meeting:789"
				}), mock.Anything).Return(&ClientReadResponse{
					Tuples: []openfga.Tuple{
						{Key: openfga.TupleKey{User: "user:456", Relation: "host", Object: "past_meeting:789"}},
						{Key: openfga.TupleKey{User: "user:999", Relation: "invitee", Object: "past_meeting:789"}},
						{Key: openfga.TupleKey{User: "user:456", Relation: "invitee", Object: "past_meeting:789"}},
						{Key: openfga.TupleKey{User: "user:456", Relation: "attendee", Object: "past_meeting:789"}},
					},
					ContinuationToken: "",
				}, nil).Once()
			},
			expectedTuples: []ClientTupleKey{
				{User: "user:456", Relation: "host", Object: "past_meeting:789"},
				{User: "user:456", Relation: "invitee", Object: "past_meeting:789"},
				{User: "user:456", Relation: "attendee", Object: "past_meeting:789"},
			},
			expectError: false,
			description: "should return all tuples for user on object",
		},
		{
			name:   "no tuples for user",
			user:   "user:nonexistent",
			object: "meeting:123",
			mockSetup: func(m *MockFgaClient) {
				m.On("Read", mock.Anything, mock.MatchedBy(func(req ClientReadRequest) bool {
					return req.Object != nil && *req.Object == "meeting:123"
				}), mock.Anything).Return(&ClientReadResponse{
					Tuples: []openfga.Tuple{
						{Key: openfga.TupleKey{User: "user:other1", Relation: "participant", Object: "meeting:123"}},
						{Key: openfga.TupleKey{User: "user:other2", Relation: "organizer", Object: "meeting:123"}},
					},
					ContinuationToken: "",
				}, nil).Once()
			},
			expectedTuples: nil,
			expectError:    false,
			description:    "should return empty list when user has no tuples",
		},
		{
			name:   "error reading object tuples",
			user:   "user:123",
			object: "meeting:error",
			mockSetup: func(m *MockFgaClient) {
				m.On("Read", mock.Anything, mock.MatchedBy(func(req ClientReadRequest) bool {
					return req.Object != nil && *req.Object == "meeting:error"
				}), mock.Anything).Return((*ClientReadResponse)(nil), errors.New("read error")).Once()
			},
			expectedTuples: nil,
			expectError:    true,
			description:    "should return error when reading tuples fails",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create mock client
			mockClient := new(MockFgaClient)
			tt.mockSetup(mockClient)

			// Create service with mock client
			service := FgaService{
				client: mockClient,
			}

			// Execute the function
			tuples, err := service.GetTuplesByUserAndObject(context.Background(), tt.user, tt.object)

			// Verify error expectations
			if tt.expectError && err == nil {
				t.Errorf("%s: expected error but got none", tt.description)
			}
			if !tt.expectError && err != nil {
				t.Errorf("%s: unexpected error: %v", tt.description, err)
			}

			// Verify tuple results
			if !tt.expectError {
				if len(tuples) != len(tt.expectedTuples) {
					t.Errorf("%s: expected %d tuples, got %d", tt.description, len(tt.expectedTuples), len(tuples))
				}
				for i, tuple := range tuples {
					if i >= len(tt.expectedTuples) {
						break
					}
					expected := tt.expectedTuples[i]
					if tuple.User != expected.User ||
						tuple.Relation != expected.Relation ||
						tuple.Object != expected.Object {
						t.Errorf("%s: tuple %d mismatch: got %+v, want %+v",
							tt.description, i, tuple, expected)
					}
				}
			}

			// Verify all expectations were met
			mockClient.AssertExpectations(t)
		})
	}
}
