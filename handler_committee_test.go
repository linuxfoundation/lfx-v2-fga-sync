// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package main

import (
	"testing"

	openfga "github.com/openfga/go-sdk"
	. "github.com/openfga/go-sdk/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

// TestCommitteeMemberPutHandler tests the committeeMemberPutHandler function
func TestCommitteeMemberPutHandler(t *testing.T) {
	tests := []struct {
		name           string
		messageData    []byte
		replySubject   string
		setupMocks     func(*HandlerService, *MockNatsMsg)
		expectedError  bool
		expectedCalled bool
	}{
		{
			name: "put new committee member",
			messageData: mustJSON(committeeMemberStub{
				Username:     "user-123",
				CommitteeUID: "committee-456",
			}),
			replySubject: "reply.subject",
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				msg.On("Respond", []byte("OK")).Return(nil).Once()

				// Mock the Read operation to check existing relations (return empty - new member)
				service.fgaService.client.(*MockFgaClient).On("Read", mock.Anything, mock.MatchedBy(func(req ClientReadRequest) bool {
					return req.Object != nil && *req.Object == "committee:committee-456"
				}), mock.Anything).Return(&ClientReadResponse{
					Tuples:            []openfga.Tuple{},
					ContinuationToken: "",
				}, nil).Once()

				// Mock the WriteTuples operation for member relation
				service.fgaService.client.(*MockFgaClient).On("Write", mock.Anything, mock.MatchedBy(func(req ClientWriteRequest) bool {
					return len(req.Writes) == 1 && len(req.Deletes) == 0 &&
						req.Writes[0].User == "user:user-123" &&
						req.Writes[0].Relation == "member" &&
						req.Writes[0].Object == "committee:committee-456"
				})).Return(&ClientWriteResponse{}, nil).Once()

				// Mock cache operations
				service.fgaService.cacheBucket.(*MockKeyValue).On("Put", mock.Anything, "inv", mock.Anything).Return(uint64(1), nil).Once()
			},
			expectedError:  false,
			expectedCalled: true,
		},
		{
			name: "put committee member without reply",
			messageData: mustJSON(committeeMemberStub{
				Username:     "user-789",
				CommitteeUID: "committee-abc",
			}),
			replySubject: "",
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				// No reply expected

				// Mock the Read operation to check existing relations (return empty - new member)
				service.fgaService.client.(*MockFgaClient).On("Read", mock.Anything, mock.MatchedBy(func(req ClientReadRequest) bool {
					return req.Object != nil && *req.Object == "committee:committee-abc"
				}), mock.Anything).Return(&ClientReadResponse{
					Tuples:            []openfga.Tuple{},
					ContinuationToken: "",
				}, nil).Once()

				// Mock the WriteTuples operation for member relation
				service.fgaService.client.(*MockFgaClient).On("Write", mock.Anything, mock.MatchedBy(func(req ClientWriteRequest) bool {
					return len(req.Writes) == 1 && len(req.Deletes) == 0 &&
						req.Writes[0].User == "user:user-789" &&
						req.Writes[0].Relation == "member" &&
						req.Writes[0].Object == "committee:committee-abc"
				})).Return(&ClientWriteResponse{}, nil).Once()

				// Mock cache operations
				service.fgaService.cacheBucket.(*MockKeyValue).On("Put", mock.Anything, "inv", mock.Anything).Return(uint64(1), nil).Once()
			},
			expectedError:  false,
			expectedCalled: false,
		},
		{
			name: "put member - already exists (no changes)",
			messageData: mustJSON(committeeMemberStub{
				Username:     "existing-user",
				CommitteeUID: "committee-789",
			}),
			replySubject: "",
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				// No reply expected

				// Mock the Read operation to return existing member relation
				service.fgaService.client.(*MockFgaClient).On("Read", mock.Anything, mock.MatchedBy(func(req ClientReadRequest) bool {
					return req.Object != nil && *req.Object == "committee:committee-789"
				}), mock.Anything).Return(&ClientReadResponse{
					Tuples: []openfga.Tuple{
						{Key: openfga.TupleKey{User: "user:existing-user", Relation: "member", Object: "committee:committee-789"}},
					},
					ContinuationToken: "",
				}, nil).Once()

				// No WriteTuples call expected since member already exists
			},
			expectedError:  false,
			expectedCalled: false,
		},
		{
			name:         "invalid JSON",
			messageData:  []byte("invalid-json"),
			replySubject: "reply.subject",
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				// No mocks needed - should fail at JSON parsing
			},
			expectedError:  true,
			expectedCalled: false,
		},
		{
			name: "missing username",
			messageData: mustJSON(committeeMemberStub{
				Username:     "",
				CommitteeUID: "committee-456",
			}),
			replySubject: "reply.subject",
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				// No mocks needed - should fail at validation
			},
			expectedError:  true,
			expectedCalled: false,
		},
		{
			name: "missing committee UID",
			messageData: mustJSON(committeeMemberStub{
				Username:     "user-123",
				CommitteeUID: "",
			}),
			replySubject: "reply.subject",
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				// No mocks needed - should fail at validation
			},
			expectedError:  true,
			expectedCalled: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create test service and message
			msg := CreateMockNatsMsg(tt.messageData)
			msg.reply = tt.replySubject

			handlerService := setupService()

			// Setup mocks if provided
			if tt.setupMocks != nil {
				tt.setupMocks(handlerService, msg)
			}

			// Execute the handler
			err := handlerService.committeeMemberPutHandler(msg)

			// Verify results
			if tt.expectedError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}

			if tt.expectedCalled {
				msg.AssertExpectations(t)
			} else {
				msg.AssertNotCalled(t, "Respond")
			}
		})
	}
}

// TestCommitteeMemberRemoveHandler tests the committeeMemberRemoveHandler function
func TestCommitteeMemberRemoveHandler(t *testing.T) {
	tests := []struct {
		name           string
		messageData    []byte
		replySubject   string
		setupMocks     func(*HandlerService, *MockNatsMsg)
		expectedError  bool
		expectedCalled bool
	}{
		{
			name: "remove committee member",
			messageData: mustJSON(committeeMemberStub{
				Username:     "user-123",
				CommitteeUID: "committee-456",
			}),
			replySubject: "reply.subject",
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				msg.On("Respond", []byte("OK")).Return(nil).Once()

				// Mock the DeleteTuple operation for member relation
				service.fgaService.client.(*MockFgaClient).On("Write", mock.Anything, mock.MatchedBy(func(req ClientWriteRequest) bool {
					return len(req.Writes) == 0 && len(req.Deletes) == 1 &&
						req.Deletes[0].User == "user:user-123" &&
						req.Deletes[0].Relation == "member" &&
						req.Deletes[0].Object == "committee:committee-456"
				})).Return(&ClientWriteResponse{}, nil).Once()

				// Mock cache operations
				service.fgaService.cacheBucket.(*MockKeyValue).On("Put", mock.Anything, "inv", mock.Anything).Return(uint64(1), nil).Once()
			},
			expectedError:  false,
			expectedCalled: true,
		},
		{
			name: "remove committee member without reply",
			messageData: mustJSON(committeeMemberStub{
				Username:     "user-789",
				CommitteeUID: "committee-abc",
			}),
			replySubject: "",
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				// No reply expected

				// Mock the DeleteTuple operation for member relation
				service.fgaService.client.(*MockFgaClient).On("Write", mock.Anything, mock.MatchedBy(func(req ClientWriteRequest) bool {
					return len(req.Writes) == 0 && len(req.Deletes) == 1 &&
						req.Deletes[0].User == "user:user-789" &&
						req.Deletes[0].Relation == "member" &&
						req.Deletes[0].Object == "committee:committee-abc"
				})).Return(&ClientWriteResponse{}, nil).Once()

				// Mock cache operations
				service.fgaService.cacheBucket.(*MockKeyValue).On("Put", mock.Anything, "inv", mock.Anything).Return(uint64(1), nil).Once()
			},
			expectedError:  false,
			expectedCalled: false,
		},
		{
			name:         "invalid JSON",
			messageData:  []byte("invalid-json"),
			replySubject: "reply.subject",
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				// No mocks needed - should fail at JSON parsing
			},
			expectedError:  true,
			expectedCalled: false,
		},
		{
			name: "missing username",
			messageData: mustJSON(committeeMemberStub{
				Username:     "",
				CommitteeUID: "committee-456",
			}),
			replySubject: "reply.subject",
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				// No mocks needed - should fail at validation
			},
			expectedError:  true,
			expectedCalled: false,
		},
		{
			name: "missing committee UID",
			messageData: mustJSON(committeeMemberStub{
				Username:     "user-123",
				CommitteeUID: "",
			}),
			replySubject: "reply.subject",
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				// No mocks needed - should fail at validation
			},
			expectedError:  true,
			expectedCalled: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create test service and message
			msg := CreateMockNatsMsg(tt.messageData)
			msg.reply = tt.replySubject

			handlerService := setupService()

			// Setup mocks if provided
			if tt.setupMocks != nil {
				tt.setupMocks(handlerService, msg)
			}

			// Execute the handler
			err := handlerService.committeeMemberRemoveHandler(msg)

			// Verify results
			if tt.expectedError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}

			if tt.expectedCalled {
				msg.AssertExpectations(t)
			} else {
				msg.AssertNotCalled(t, "Respond")
			}
		})
	}
}
