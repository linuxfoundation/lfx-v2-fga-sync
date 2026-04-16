// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package main

import (
	"encoding/json"
	"errors"
	"testing"

	openfga "github.com/openfga/go-sdk"
	"github.com/openfga/go-sdk/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	"github.com/linuxfoundation/lfx-v2-fga-sync/pkg/types"
)

// TestReadUserTuples tests the ReadUserTuples method of FgaService.
func TestReadUserTuples(t *testing.T) {
	tests := []struct {
		name           string
		user           string
		objectType     string
		mockSetup      func(*MockFgaClient)
		expectedTuples []openfga.Tuple
		expectError    bool
		description    string
	}{
		{
			name:       "single page of tuples",
			user:       "user:auth0|alice",
			objectType: "project",
			mockSetup: func(m *MockFgaClient) {
				m.On("Read", mock.Anything, mock.MatchedBy(func(req client.ClientReadRequest) bool {
					return req.User != nil && *req.User == "user:auth0|alice" &&
						req.Object != nil && *req.Object == "project:"
				}), mock.Anything).Return(&client.ClientReadResponse{
					Tuples: []openfga.Tuple{
						{Key: openfga.TupleKey{Object: "project:uuid1", Relation: "writer", User: "user:auth0|alice"}},
						{Key: openfga.TupleKey{Object: "project:uuid2", Relation: "auditor", User: "user:auth0|alice"}},
					},
					ContinuationToken: "",
				}, nil).Once()
			},
			expectedTuples: []openfga.Tuple{
				{Key: openfga.TupleKey{Object: "project:uuid1", Relation: "writer", User: "user:auth0|alice"}},
				{Key: openfga.TupleKey{Object: "project:uuid2", Relation: "auditor", User: "user:auth0|alice"}},
			},
			expectError: false,
			description: "should return all tuples from single page",
		},
		{
			name:       "multiple pages with pagination",
			user:       "user:auth0|bob",
			objectType: "committee",
			mockSetup: func(m *MockFgaClient) {
				// First page.
				m.On("Read", mock.Anything, mock.MatchedBy(func(req client.ClientReadRequest) bool {
					return req.User != nil && *req.User == "user:auth0|bob" &&
						req.Object != nil && *req.Object == "committee:"
				}), mock.MatchedBy(func(opts client.ClientReadOptions) bool {
					return opts.ContinuationToken == nil
				})).Return(&client.ClientReadResponse{
					Tuples: []openfga.Tuple{
						{Key: openfga.TupleKey{Object: "committee:c1", Relation: "member", User: "user:auth0|bob"}},
					},
					ContinuationToken: "page-2",
				}, nil).Once()
				// Second page.
				m.On("Read", mock.Anything, mock.MatchedBy(func(req client.ClientReadRequest) bool {
					return req.User != nil && *req.User == "user:auth0|bob" &&
						req.Object != nil && *req.Object == "committee:"
				}), mock.MatchedBy(func(opts client.ClientReadOptions) bool {
					return opts.ContinuationToken != nil && *opts.ContinuationToken == "page-2"
				})).Return(&client.ClientReadResponse{
					Tuples: []openfga.Tuple{
						{Key: openfga.TupleKey{Object: "committee:c2", Relation: "member", User: "user:auth0|bob"}},
					},
					ContinuationToken: "",
				}, nil).Once()
			},
			expectedTuples: []openfga.Tuple{
				{Key: openfga.TupleKey{Object: "committee:c1", Relation: "member", User: "user:auth0|bob"}},
				{Key: openfga.TupleKey{Object: "committee:c2", Relation: "member", User: "user:auth0|bob"}},
			},
			expectError: false,
			description: "should aggregate tuples across pages",
		},
		{
			name:       "empty result",
			user:       "user:auth0|nobody",
			objectType: "project",
			mockSetup: func(m *MockFgaClient) {
				m.On("Read", mock.Anything, mock.Anything, mock.Anything).Return(&client.ClientReadResponse{
					Tuples:            []openfga.Tuple{},
					ContinuationToken: "",
				}, nil).Once()
			},
			expectedTuples: []openfga.Tuple{},
			expectError:    false,
			description:    "should handle empty result without error",
		},
		{
			name:       "OpenFGA error",
			user:       "user:auth0|err",
			objectType: "project",
			mockSetup: func(m *MockFgaClient) {
				m.On("Read", mock.Anything, mock.Anything, mock.Anything).Return(
					(*client.ClientReadResponse)(nil), errors.New("openfga unavailable"),
				).Once()
			},
			expectedTuples: nil,
			expectError:    true,
			description:    "should propagate OpenFGA errors",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockClient := &MockFgaClient{}
			tt.mockSetup(mockClient)
			svc := FgaService{client: mockClient, cacheBucket: NewMockKeyValue()}

			tuples, err := svc.ReadUserTuples(t.Context(), tt.user, tt.objectType)

			if tt.expectError {
				assert.Error(t, err, tt.description)
			} else {
				assert.NoError(t, err, tt.description)
				assert.Equal(t, len(tt.expectedTuples), len(tuples), tt.description)
			}
			mockClient.AssertExpectations(t)
		})
	}
}

// TestReadTuplesHandler tests the readTuplesHandler function.
func TestReadTuplesHandler(t *testing.T) {
	tests := []struct {
		name         string
		messageData  []byte
		replySubject string
		mockSetup    func(*MockFgaClient, *MockNatsMsg)
		expectError  bool
	}{
		{
			name:         "success returns tuple-strings",
			messageData:  []byte(`{"user":"user:auth0|alice","object_type":"project"}`),
			replySubject: "reply.123",
			mockSetup: func(m *MockFgaClient, msg *MockNatsMsg) {
				m.On("Read", mock.Anything, mock.MatchedBy(func(req client.ClientReadRequest) bool {
					return req.User != nil && *req.User == "user:auth0|alice" &&
						req.Object != nil && *req.Object == "project:"
				}), mock.Anything).Return(&client.ClientReadResponse{
					Tuples: []openfga.Tuple{
						{Key: openfga.TupleKey{Object: "project:uuid1", Relation: "writer", User: "user:auth0|alice"}},
					},
					ContinuationToken: "",
				}, nil).Once()
				msg.On("Respond", mock.MatchedBy(func(data []byte) bool {
					var resp types.ReadTuplesResponse
					if err := json.Unmarshal(data, &resp); err != nil {
						return false
					}
					return len(resp.Results) == 1 &&
						resp.Results[0] == "project:uuid1#writer@user:auth0|alice" &&
						resp.Error == ""
				})).Return(nil).Once()
			},
			expectError: false,
		},
		{
			name:         "empty results returned as empty array",
			messageData:  []byte(`{"user":"user:auth0|nobody","object_type":"project"}`),
			replySubject: "reply.456",
			mockSetup: func(m *MockFgaClient, msg *MockNatsMsg) {
				m.On("Read", mock.Anything, mock.Anything, mock.Anything).Return(&client.ClientReadResponse{
					Tuples:            []openfga.Tuple{},
					ContinuationToken: "",
				}, nil).Once()
				msg.On("Respond", mock.MatchedBy(func(data []byte) bool {
					var resp types.ReadTuplesResponse
					if err := json.Unmarshal(data, &resp); err != nil {
						return false
					}
					// Results should be present as empty array, not nil.
					return resp.Results != nil && len(resp.Results) == 0 && resp.Error == ""
				})).Return(nil).Once()
			},
			expectError: false,
		},
		{
			name:         "OpenFGA error returns generic JSON error",
			messageData:  []byte(`{"user":"user:auth0|err","object_type":"project"}`),
			replySubject: "reply.err",
			mockSetup: func(m *MockFgaClient, msg *MockNatsMsg) {
				m.On("Read", mock.Anything, mock.Anything, mock.Anything).Return(
					(*client.ClientReadResponse)(nil), errors.New("store unavailable"),
				).Once()
				msg.On("Respond", mock.MatchedBy(func(data []byte) bool {
					var resp types.ReadTuplesResponse
					if err := json.Unmarshal(data, &resp); err != nil {
						return false
					}
					// Error message should not contain internal details.
					return resp.Error == "failed to read tuples"
				})).Return(nil).Once()
			},
			expectError: true, // Handler now returns error for subscription-layer logging.
		},
		{
			name:         "invalid JSON payload returns error response",
			messageData:  []byte(`not-json`),
			replySubject: "reply.bad",
			mockSetup: func(_ *MockFgaClient, msg *MockNatsMsg) {
				msg.On("Respond", mock.MatchedBy(func(data []byte) bool {
					var resp types.ReadTuplesResponse
					if err := json.Unmarshal(data, &resp); err != nil {
						return false
					}
					// Should not leak unmarshal error details.
					return resp.Error == "invalid request payload"
				})).Return(nil).Once()
			},
			expectError: true,
		},
		{
			name:         "missing user field returns error response",
			messageData:  []byte(`{"object_type":"project"}`),
			replySubject: "reply.no-user",
			mockSetup: func(_ *MockFgaClient, msg *MockNatsMsg) {
				msg.On("Respond", mock.MatchedBy(func(data []byte) bool {
					var resp types.ReadTuplesResponse
					if err := json.Unmarshal(data, &resp); err != nil {
						return false
					}
					return resp.Error != ""
				})).Return(nil).Once()
			},
			expectError: true,
		},
		{
			name:         "object_type with colon is rejected",
			messageData:  []byte(`{"user":"user:auth0|alice","object_type":"project:"}`),
			replySubject: "reply.colon",
			mockSetup: func(_ *MockFgaClient, msg *MockNatsMsg) {
				msg.On("Respond", mock.MatchedBy(func(data []byte) bool {
					var resp types.ReadTuplesResponse
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
			messageData:  []byte(`{"user":"user:auth0|alice","object_type":"project"}`),
			replySubject: "",
			mockSetup: func(m *MockFgaClient, _ *MockNatsMsg) {
				m.On("Read", mock.Anything, mock.Anything, mock.Anything).Return(&client.ClientReadResponse{
					Tuples:            []openfga.Tuple{},
					ContinuationToken: "",
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
				err := service.readTuplesHandler(msg)
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
