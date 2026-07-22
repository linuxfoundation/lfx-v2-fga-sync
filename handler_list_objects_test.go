// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package main provides the fga-sync service entry point and supporting types.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/openfga/go-sdk/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	"github.com/linuxfoundation/lfx-v2-fga-sync/pkg/types"
)

// TestListObjectsCached tests the ListObjectsCached method of FgaService.
func TestListObjectsCached(t *testing.T) {
	tests := []struct {
		name            string
		objectType      string
		relation        string
		user            string
		useCacheValue   bool
		mockClientSetup func(*MockFgaClient)
		mockCacheSetup  func(*MockKeyValue)
		expectedResult  []string
		expectError     bool
		description     string
	}{
		{
			name:          "cache disabled queries OpenFGA directly",
			objectType:    "project",
			relation:      "writer",
			user:          "user:auth0|alice",
			useCacheValue: false,
			mockClientSetup: func(m *MockFgaClient) {
				m.On("ListObjects", mock.Anything, mock.MatchedBy(func(req client.ClientListObjectsRequest) bool {
					return req.User == "user:auth0|alice" && req.Relation == "writer" && req.Type == "project"
				}), mock.Anything).Return(&client.ClientListObjectsResponse{
					Objects: []string{"project:uuid1"},
				}, nil).Once()
			},
			mockCacheSetup: func(_ *MockKeyValue) {},
			expectedResult: []string{"project:uuid1"},
			expectError:    false,
			description:    "should bypass cache entirely when useCache is false",
		},
		{
			name:          "cache enabled, cache miss queries OpenFGA and caches result",
			objectType:    "committee",
			relation:      "writer",
			user:          "user:auth0|bob",
			useCacheValue: true,
			mockClientSetup: func(m *MockFgaClient) {
				m.On("ListObjects", mock.Anything, mock.MatchedBy(func(req client.ClientListObjectsRequest) bool {
					return req.User == "user:auth0|bob" && req.Relation == "writer" && req.Type == "committee"
				}), mock.Anything).Return(&client.ClientListObjectsResponse{
					Objects: []string{"committee:abc"},
				}, nil).Once()
			},
			mockCacheSetup: func(m *MockKeyValue) {
				m.On("Get", mock.Anything, "inv").Return(nil, jetstream.ErrKeyNotFound)
				m.SetNotFound("list.OVZWK4R2MF2XI2BQPRRG6YRDO5ZGS5DFOJAGG33NNVUXI5DFMU")
			},
			expectedResult: []string{"committee:abc"},
			expectError:    false,
			description:    "should query OpenFGA and cache the fresh result on a cache miss",
		},
		{
			name:          "cache enabled, fresh cache hit skips OpenFGA",
			objectType:    "project",
			relation:      "writer",
			user:          "user:auth0|carol",
			useCacheValue: true,
			mockClientSetup: func(_ *MockFgaClient) {
				// No ListObjects call expected; a fresh cache hit must short-circuit.
			},
			mockCacheSetup: func(m *MockKeyValue) {
				m.On("Get", mock.Anything, "inv").Return(nil, jetstream.ErrKeyNotFound)
			},
			expectedResult: []string{"project:cached-uuid"},
			expectError:    false,
			description:    "should return the cached value without calling OpenFGA",
		},
		{
			name:          "cache enabled, stale cache entry re-queries OpenFGA",
			objectType:    "project",
			relation:      "writer",
			user:          "user:auth0|dave",
			useCacheValue: true,
			mockClientSetup: func(m *MockFgaClient) {
				m.On("ListObjects", mock.Anything, mock.Anything, mock.Anything).Return(&client.ClientListObjectsResponse{
					Objects: []string{"project:fresh-uuid"},
				}, nil).Once()
			},
			mockCacheSetup: func(_ *MockKeyValue) {},
			expectedResult: []string{"project:fresh-uuid"},
			expectError:    false,
			description:    "should ignore a cache entry older than the last invalidation",
		},
		{
			name:          "OpenFGA error propagates",
			objectType:    "project",
			relation:      "writer",
			user:          "user:auth0|err",
			useCacheValue: false,
			mockClientSetup: func(m *MockFgaClient) {
				m.On("ListObjects", mock.Anything, mock.Anything, mock.Anything).Return(
					(*client.ClientListObjectsResponse)(nil), errors.New("openfga unavailable"),
				).Once()
			},
			mockCacheSetup: func(_ *MockKeyValue) {},
			expectedResult: nil,
			expectError:    true,
			description:    "should propagate OpenFGA errors",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			useCache = tt.useCacheValue
			t.Cleanup(func() { useCache = false })

			mockClient := &MockFgaClient{}
			tt.mockClientSetup(mockClient)
			mockCache := NewMockKeyValue()
			tt.mockCacheSetup(mockCache)

			svc := FgaService{client: mockClient, cacheBucket: mockCache}

			// Pre-seed a fresh cache entry for the "fresh cache hit" case, and a
			// stale one (before the invalidation marker) for the "stale" case.
			switch tt.name {
			case "cache enabled, fresh cache hit skips OpenFGA":
				data, err := json.Marshal(tt.expectedResult)
				assert.NoError(t, err)
				_, err = mockCache.Put(t.Context(), "list.OVZWK4R2MF2XI2BQPRRWC4TPNQRXO4TJORSXEQDQOJXWUZLDOQ", data)
				assert.NoError(t, err)
			case "cache enabled, stale cache entry re-queries OpenFGA":
				data, err := json.Marshal([]string{"project:stale-uuid"})
				assert.NoError(t, err)
				_, err = mockCache.Put(t.Context(), "list.OVZWK4R2MF2XI2BQPRSGC5TFEN3XE2LUMVZEA4DSN5VGKY3U", data)
				assert.NoError(t, err)
				_, err = mockCache.Put(t.Context(), "inv", []byte("1"))
				assert.NoError(t, err)
			}

			objects, err := svc.ListObjectsCached(t.Context(), tt.objectType, tt.relation, tt.user)

			if tt.expectError {
				assert.Error(t, err, tt.description)
			} else {
				assert.NoError(t, err, tt.description)
				assert.Equal(t, tt.expectedResult, objects, tt.description)
			}
			mockClient.AssertExpectations(t)
		})
	}
}

// TestListObjectsHandler tests the listObjectsHandler function.
func TestListObjectsHandler(t *testing.T) {
	tests := []struct {
		name         string
		messageData  []byte
		replySubject string
		mockSetup    func(*MockFgaClient, *MockNatsMsg)
		expectError  bool
	}{
		{
			name:         "success returns annotated targets",
			messageData:  []byte(`{"user":"user:auth0|alice","queries":[{"object_type":"project","relation":"writer"}]}`),
			replySubject: "reply.123",
			mockSetup: func(m *MockFgaClient, msg *MockNatsMsg) {
				m.On("ListObjects", mock.Anything, mock.MatchedBy(func(req client.ClientListObjectsRequest) bool {
					return req.User == "user:auth0|alice" && req.Relation == "writer" && req.Type == "project"
				}), mock.Anything).Return(&client.ClientListObjectsResponse{
					Objects: []string{"project:uuid1"},
				}, nil).Once()
				msg.On("Respond", mock.MatchedBy(func(data []byte) bool {
					var resp types.ListObjectsResponse
					if err := json.Unmarshal(data, &resp); err != nil {
						return false
					}
					return len(resp.Targets) == 1 &&
						resp.Targets[0].Object == "project:uuid1" &&
						resp.Targets[0].ObjectType == "project" &&
						resp.Targets[0].Relation == "writer" &&
						len(resp.Targets[0].CreatableTypes) > 0 &&
						resp.Error == ""
				})).Return(nil).Once()
			},
			expectError: false,
		},
		{
			name:         "multiple queries aggregate into one target list",
			messageData:  []byte(`{"user":"user:auth0|alice","queries":[{"object_type":"project","relation":"writer"},{"object_type":"committee","relation":"writer"}]}`),
			replySubject: "reply.456",
			mockSetup: func(m *MockFgaClient, msg *MockNatsMsg) {
				m.On("ListObjects", mock.Anything, mock.MatchedBy(func(req client.ClientListObjectsRequest) bool {
					return req.Type == "project"
				}), mock.Anything).Return(&client.ClientListObjectsResponse{
					Objects: []string{"project:uuid1"},
				}, nil).Once()
				m.On("ListObjects", mock.Anything, mock.MatchedBy(func(req client.ClientListObjectsRequest) bool {
					return req.Type == "committee"
				}), mock.Anything).Return(&client.ClientListObjectsResponse{
					Objects: []string{"committee:abc"},
				}, nil).Once()
				msg.On("Respond", mock.MatchedBy(func(data []byte) bool {
					var resp types.ListObjectsResponse
					if err := json.Unmarshal(data, &resp); err != nil {
						return false
					}
					return len(resp.Targets) == 2 && resp.Error == ""
				})).Return(nil).Once()
			},
			expectError: false,
		},
		{
			name:         "empty results returned as empty array",
			messageData:  []byte(`{"user":"user:auth0|nobody","queries":[{"object_type":"project","relation":"writer"}]}`),
			replySubject: "reply.789",
			mockSetup: func(m *MockFgaClient, msg *MockNatsMsg) {
				m.On("ListObjects", mock.Anything, mock.Anything, mock.Anything).Return(&client.ClientListObjectsResponse{
					Objects: []string{},
				}, nil).Once()
				msg.On("Respond", mock.MatchedBy(func(data []byte) bool {
					var resp types.ListObjectsResponse
					if err := json.Unmarshal(data, &resp); err != nil {
						return false
					}
					return resp.Targets != nil && len(resp.Targets) == 0 && resp.Error == ""
				})).Return(nil).Once()
			},
			expectError: false,
		},
		{
			name:         "OpenFGA error returns generic JSON error",
			messageData:  []byte(`{"user":"user:auth0|err","queries":[{"object_type":"project","relation":"writer"}]}`),
			replySubject: "reply.err",
			mockSetup: func(m *MockFgaClient, msg *MockNatsMsg) {
				m.On("ListObjects", mock.Anything, mock.Anything, mock.Anything).Return(
					(*client.ClientListObjectsResponse)(nil), errors.New("store unavailable"),
				).Once()
				msg.On("Respond", mock.MatchedBy(func(data []byte) bool {
					var resp types.ListObjectsResponse
					if err := json.Unmarshal(data, &resp); err != nil {
						return false
					}
					return resp.Error == "failed to list objects"
				})).Return(nil).Once()
			},
			expectError: true,
		},
		{
			name:         "invalid JSON payload returns error response",
			messageData:  []byte(`not-json`),
			replySubject: "reply.bad",
			mockSetup: func(_ *MockFgaClient, msg *MockNatsMsg) {
				msg.On("Respond", mock.MatchedBy(func(data []byte) bool {
					var resp types.ListObjectsResponse
					if err := json.Unmarshal(data, &resp); err != nil {
						return false
					}
					return resp.Error == "invalid request payload"
				})).Return(nil).Once()
			},
			expectError: true,
		},
		{
			name:         "missing user field returns error response",
			messageData:  []byte(`{"queries":[{"object_type":"project","relation":"writer"}]}`),
			replySubject: "reply.no-user",
			mockSetup: func(_ *MockFgaClient, msg *MockNatsMsg) {
				msg.On("Respond", mock.MatchedBy(func(data []byte) bool {
					var resp types.ListObjectsResponse
					if err := json.Unmarshal(data, &resp); err != nil {
						return false
					}
					return resp.Error != ""
				})).Return(nil).Once()
			},
			expectError: true,
		},
		{
			name:         "empty queries returns error response",
			messageData:  []byte(`{"user":"user:auth0|alice","queries":[]}`),
			replySubject: "reply.no-queries",
			mockSetup: func(_ *MockFgaClient, msg *MockNatsMsg) {
				msg.On("Respond", mock.MatchedBy(func(data []byte) bool {
					var resp types.ListObjectsResponse
					if err := json.Unmarshal(data, &resp); err != nil {
						return false
					}
					return resp.Error == "user and queries are required"
				})).Return(nil).Once()
			},
			expectError: true,
		},
		{
			name:         "query missing relation returns error response",
			messageData:  []byte(`{"user":"user:auth0|alice","queries":[{"object_type":"project"}]}`),
			replySubject: "reply.no-relation",
			mockSetup: func(_ *MockFgaClient, msg *MockNatsMsg) {
				msg.On("Respond", mock.MatchedBy(func(data []byte) bool {
					var resp types.ListObjectsResponse
					if err := json.Unmarshal(data, &resp); err != nil {
						return false
					}
					return resp.Error == "object_type and relation are required for each query"
				})).Return(nil).Once()
			},
			expectError: true,
		},
		{
			name:         "object_type with colon is rejected",
			messageData:  []byte(`{"user":"user:auth0|alice","queries":[{"object_type":"project:","relation":"writer"}]}`),
			replySubject: "reply.colon",
			mockSetup: func(_ *MockFgaClient, msg *MockNatsMsg) {
				msg.On("Respond", mock.MatchedBy(func(data []byte) bool {
					var resp types.ListObjectsResponse
					if err := json.Unmarshal(data, &resp); err != nil {
						return false
					}
					return resp.Error == "object_type must not contain ':'"
				})).Return(nil).Once()
			},
			expectError: true,
		},
		{
			name:         "no reply subject — no Respond call",
			messageData:  []byte(`{"user":"user:auth0|alice","queries":[{"object_type":"project","relation":"writer"}]}`),
			replySubject: "",
			mockSetup: func(m *MockFgaClient, _ *MockNatsMsg) {
				m.On("ListObjects", mock.Anything, mock.Anything, mock.Anything).Return(&client.ClientListObjectsResponse{
					Objects: []string{},
				}, nil).Once()
			},
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			service := setupService()
			msg := CreateMockNatsMsg(tt.messageData)
			msg.reply = tt.replySubject

			tt.mockSetup(service.fgaService.client.(*MockFgaClient), msg)

			assert.NotPanics(t, func() {
				err := service.listObjectsHandler(context.Background(), msg)
				if tt.expectError {
					assert.Error(t, err)
				} else {
					assert.NoError(t, err)
				}
			})

			msg.AssertExpectations(t)
			service.fgaService.client.(*MockFgaClient).AssertExpectations(t)
		})
	}
}
