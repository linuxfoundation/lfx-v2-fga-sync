// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package main

import (
	"encoding/json"
	"testing"

	openfga "github.com/openfga/go-sdk"
	. "github.com/openfga/go-sdk/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

// TestProjectUpdateAccessHandler tests the projectUpdateAccessHandler function
func TestProjectUpdateAccessHandler(t *testing.T) {
	tests := []struct {
		name           string
		messageData    []byte
		replySubject   string
		setupMocks     func(*HandlerService, *MockNatsMsg)
		expectedError  bool
		expectedReply  string
		expectedCalled bool
	}{
		{
			name: "valid project with all fields",
			messageData: mustJSON(projectStub{
				UID:                 "test-project-123",
				Public:              true,
				ParentUID:           "parent-project-456",
				Writers:             []string{"user1", "user2"},
				Auditors:            []string{"auditor1"},
				MeetingCoordinators: []string{"coordinator1", "coordinator2"},
			}),
			replySubject: "reply.subject",
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				msg.On("Respond", []byte("OK")).Return(nil).Once()

				// Mock the Read operation to return existing tuples
				service.fgaService.client.(*MockFgaClient).On("Read", mock.Anything, mock.MatchedBy(func(req ClientReadRequest) bool {
					return req.Object != nil && *req.Object == "project:test-project-123"
				}), mock.Anything).Return(&ClientReadResponse{
					Tuples:            []openfga.Tuple{},
					ContinuationToken: "",
				}, nil).Once()

				// Mock the Write operation
				service.fgaService.client.(*MockFgaClient).On("Write", mock.Anything, mock.MatchedBy(func(req ClientWriteRequest) bool {
					// Verify the write request contains expected tuples:
					// 1 public viewer + 1 parent + 2 writers + 1 auditor + 2 meeting coordinators = 7
					return len(req.Writes) == 7 && len(req.Deletes) == 0
				})).Return(&ClientWriteResponse{}, nil).Once()

				// Mock cache operations
				service.fgaService.cacheBucket.(*MockKeyValue).On("Put", mock.Anything, "inv", mock.Anything).Return(uint64(1), nil).Once()
				service.fgaService.cacheBucket.(*MockKeyValue).On("PutString", mock.Anything, mock.Anything, mock.Anything).Return(uint64(1), nil).Maybe()
			},
			expectedError:  false,
			expectedCalled: true,
		},
		{
			name: "private project without parent",
			messageData: mustJSON(projectStub{
				UID:                 "private-project",
				Public:              false,
				Writers:             []string{"writer1"},
				Auditors:            []string{},
				MeetingCoordinators: []string{},
			}),
			replySubject: "",
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				// No reply expected when replySubject is empty

				// Mock the Read operation
				service.fgaService.client.(*MockFgaClient).On("Read", mock.Anything, mock.MatchedBy(func(req ClientReadRequest) bool {
					return req.Object != nil && *req.Object == "project:private-project"
				}), mock.Anything).Return(&ClientReadResponse{
					Tuples:            []openfga.Tuple{},
					ContinuationToken: "",
				}, nil).Once()

				// Mock the Write operation for 1 writer
				service.fgaService.client.(*MockFgaClient).On("Write", mock.Anything, mock.MatchedBy(func(req ClientWriteRequest) bool {
					return len(req.Writes) == 1 && len(req.Deletes) == 0
				})).Return(&ClientWriteResponse{}, nil).Once()

				// Mock cache operations
				service.fgaService.cacheBucket.(*MockKeyValue).On("Put", mock.Anything, "inv", mock.Anything).Return(uint64(1), nil).Once()
				service.fgaService.cacheBucket.(*MockKeyValue).On("PutString", mock.Anything, mock.Anything, mock.Anything).Return(uint64(1), nil).Maybe()
			},
			expectedError:  false,
			expectedCalled: false,
		},
		{
			name: "project with meeting coordinators only",
			messageData: mustJSON(projectStub{
				UID:                 "coordinators-only",
				Public:              false,
				MeetingCoordinators: []string{"coord1", "coord2", "coord3"},
			}),
			replySubject: "reply.subject",
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				msg.On("Respond", []byte("OK")).Return(nil).Once()

				// Mock the Read operation
				service.fgaService.client.(*MockFgaClient).On("Read", mock.Anything, mock.MatchedBy(func(req ClientReadRequest) bool {
					return req.Object != nil && *req.Object == "project:coordinators-only"
				}), mock.Anything).Return(&ClientReadResponse{
					Tuples:            []openfga.Tuple{},
					ContinuationToken: "",
				}, nil).Once()

				// Mock the Write operation for 3 meeting coordinators
				service.fgaService.client.(*MockFgaClient).On("Write", mock.Anything, mock.MatchedBy(func(req ClientWriteRequest) bool {
					return len(req.Writes) == 3 && len(req.Deletes) == 0
				})).Return(&ClientWriteResponse{}, nil).Once()

				// Mock cache operations
				service.fgaService.cacheBucket.(*MockKeyValue).On("Put", mock.Anything, "inv", mock.Anything).Return(uint64(1), nil).Once()
				service.fgaService.cacheBucket.(*MockKeyValue).On("PutString", mock.Anything, mock.Anything, mock.Anything).Return(uint64(1), nil).Maybe()
			},
			expectedError:  false,
			expectedCalled: true,
		},
		{
			name: "public project with no users",
			messageData: mustJSON(projectStub{
				UID:    "public-empty",
				Public: true,
			}),
			replySubject: "reply.subject",
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				msg.On("Respond", []byte("OK")).Return(nil).Once()

				// Mock the Read operation
				service.fgaService.client.(*MockFgaClient).On("Read", mock.Anything, mock.MatchedBy(func(req ClientReadRequest) bool {
					return req.Object != nil && *req.Object == "project:public-empty"
				}), mock.Anything).Return(&ClientReadResponse{
					Tuples:            []openfga.Tuple{},
					ContinuationToken: "",
				}, nil).Once()

				// Mock the Write operation for public viewer
				service.fgaService.client.(*MockFgaClient).On("Write", mock.Anything, mock.MatchedBy(func(req ClientWriteRequest) bool {
					return len(req.Writes) == 1 && len(req.Deletes) == 0 &&
						req.Writes[0].User == "user:*" && req.Writes[0].Relation == "viewer"
				})).Return(&ClientWriteResponse{}, nil).Once()

				// Mock cache operations
				service.fgaService.cacheBucket.(*MockKeyValue).On("Put", mock.Anything, "inv", mock.Anything).Return(uint64(1), nil).Once()
			},
			expectedError:  false,
			expectedCalled: true,
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
			name:         "empty project UID",
			messageData:  mustJSON(projectStub{}),
			replySubject: "reply.subject",
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				// No mocks needed - should fail at UID validation
			},
			expectedError:  true,
			expectedCalled: false,
		},
		{
			name:         "empty message",
			messageData:  []byte(""),
			replySubject: "reply.subject",
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				// No mocks needed - should fail at JSON parsing
			},
			expectedError:  true,
			expectedCalled: false,
		},
		{
			name: "project with empty arrays",
			messageData: mustJSON(projectStub{
				UID:      "empty-arrays",
				Writers:  []string{},
				Auditors: []string{},
			}),
			replySubject: "",
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				// Mock the Read operation - no existing tuples
				service.fgaService.client.(*MockFgaClient).On("Read", mock.Anything, mock.MatchedBy(func(req ClientReadRequest) bool {
					return req.Object != nil && *req.Object == "project:empty-arrays"
				}), mock.Anything).Return(&ClientReadResponse{
					Tuples:            []openfga.Tuple{},
					ContinuationToken: "",
				}, nil).Once()
				// No Write operation expected since there are no tuples to write
			},
			expectedError:  false,
			expectedCalled: false,
		},
		{
			name: "respond error handling",
			messageData: mustJSON(projectStub{
				UID:    "respond-error",
				Public: true,
			}),
			replySubject: "reply.subject",
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				msg.On("Respond", []byte("OK")).Return(assert.AnError).Once()

				// Mock the Read and Write operations
				service.fgaService.client.(*MockFgaClient).On("Read", mock.Anything, mock.Anything, mock.Anything).Return(&ClientReadResponse{
					Tuples:            []openfga.Tuple{},
					ContinuationToken: "",
				}, nil).Once()
				service.fgaService.client.(*MockFgaClient).On("Write", mock.Anything, mock.Anything).Return(&ClientWriteResponse{}, nil).Once()
				service.fgaService.cacheBucket.(*MockKeyValue).On("Put", mock.Anything, "inv", mock.Anything).Return(uint64(1), nil).Once()
			},
			expectedError:  true,
			expectedCalled: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := CreateMockNatsMsg(tt.messageData)
			msg.reply = tt.replySubject

			handlerService := setupService()
			tt.setupMocks(handlerService, msg)

			// Test that the function doesn't panic
			assert.NotPanics(t, func() {
				err := handlerService.projectUpdateAccessHandler(msg)
				if tt.expectedError {
					assert.Error(t, err)
				} else {
					assert.NoError(t, err)
				}
			})

			// Verify mock expectations
			if tt.expectedCalled {
				msg.AssertExpectations(t)
			} else {
				// Ensure Respond was never called
				msg.AssertNotCalled(t, "Respond")
			}
		})
	}
}

// TestProjectDeleteAllAccessHandler tests the projectDeleteAllAccessHandler function
func TestProjectDeleteAllAccessHandler(t *testing.T) {
	tests := []struct {
		name           string
		messageData    []byte
		replySubject   string
		setupMocks     func(*HandlerService, *MockNatsMsg)
		expectedError  bool
		expectedReply  string
		expectedCalled bool
	}{
		{
			name:         "valid project UID",
			messageData:  []byte("test-project-123"),
			replySubject: "reply.subject",
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				msg.On("Respond", []byte("OK")).Return(nil).Once()

				// Mock the Read operation to return some existing tuples
				service.fgaService.client.(*MockFgaClient).On("Read", mock.Anything, mock.MatchedBy(func(req ClientReadRequest) bool {
					return req.Object != nil && *req.Object == "project:test-project-123"
				}), mock.Anything).Return(&ClientReadResponse{
					Tuples: []openfga.Tuple{
						{Key: openfga.TupleKey{User: "user:456", Relation: "admin", Object: "project:test-project-123"}},
						{Key: openfga.TupleKey{User: "user:789", Relation: "viewer", Object: "project:test-project-123"}},
					},
					ContinuationToken: "",
				}, nil).Once()

				// Mock the Write operation to delete all tuples
				service.fgaService.client.(*MockFgaClient).On("Write", mock.Anything, mock.MatchedBy(func(req ClientWriteRequest) bool {
					// Should have no writes and 2 deletes
					return len(req.Writes) == 0 && len(req.Deletes) == 2
				})).Return(&ClientWriteResponse{}, nil).Once()

				// Mock cache invalidation
				service.fgaService.cacheBucket.(*MockKeyValue).On("Put", mock.Anything, "inv", mock.Anything).Return(uint64(1), nil).Once()
			},
			expectedError:  false,
			expectedCalled: true,
		},
		{
			name:         "empty payload",
			messageData:  []byte(""),
			replySubject: "reply.subject",
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				// No mocks needed - should fail at validation
			},
			expectedError:  true,
			expectedCalled: false,
		},
		{
			name:         "JSON object payload",
			messageData:  []byte(`{"uid": "test"}`),
			replySubject: "reply.subject",
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				// No mocks needed - should fail at validation
			},
			expectedError:  true,
			expectedCalled: false,
		},
		{
			name:         "JSON array payload",
			messageData:  []byte(`["test"]`),
			replySubject: "reply.subject",
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				// No mocks needed - should fail at validation
			},
			expectedError:  true,
			expectedCalled: false,
		},
		{
			name:         "quoted string payload",
			messageData:  []byte(`"test-project"`),
			replySubject: "reply.subject",
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				// No mocks needed - should fail at validation
			},
			expectedError:  true,
			expectedCalled: false,
		},
		{
			name:         "project UID without reply",
			messageData:  []byte("project-no-reply"),
			replySubject: "",
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				// No reply expected when replySubject is empty

				// Mock the Read operation
				service.fgaService.client.(*MockFgaClient).On("Read", mock.Anything, mock.MatchedBy(func(req ClientReadRequest) bool {
					return req.Object != nil && *req.Object == "project:project-no-reply"
				}), mock.Anything).Return(&ClientReadResponse{
					Tuples:            []openfga.Tuple{},
					ContinuationToken: "",
				}, nil).Once()
				// No Write operation expected since there are no tuples to delete
			},
			expectedError:  false,
			expectedCalled: false,
		},
		{
			name:         "project with existing tuples and no reply",
			messageData:  []byte("project-with-tuples"),
			replySubject: "",
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				// Mock the Read operation to return existing tuples
				service.fgaService.client.(*MockFgaClient).On("Read", mock.Anything, mock.MatchedBy(func(req ClientReadRequest) bool {
					return req.Object != nil && *req.Object == "project:project-with-tuples"
				}), mock.Anything).Return(&ClientReadResponse{
					Tuples: []openfga.Tuple{
						{Key: openfga.TupleKey{User: "user:100", Relation: "writer", Object: "project:project-with-tuples"}},
					},
					ContinuationToken: "",
				}, nil).Once()

				// Mock the Write operation to delete the tuple
				service.fgaService.client.(*MockFgaClient).On("Write", mock.Anything, mock.MatchedBy(func(req ClientWriteRequest) bool {
					return len(req.Writes) == 0 && len(req.Deletes) == 1
				})).Return(&ClientWriteResponse{}, nil).Once()

				// Mock cache invalidation
				service.fgaService.cacheBucket.(*MockKeyValue).On("Put", mock.Anything, "inv", mock.Anything).Return(uint64(1), nil).Once()
			},
			expectedError:  false,
			expectedCalled: false,
		},
		{
			name:         "respond error handling",
			messageData:  []byte("error-project"),
			replySubject: "reply.subject",
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				msg.On("Respond", []byte("OK")).Return(assert.AnError).Once()

				// Mock the Read and Write operations
				service.fgaService.client.(*MockFgaClient).On("Read", mock.Anything, mock.Anything, mock.Anything).Return(&ClientReadResponse{
					Tuples:            []openfga.Tuple{},
					ContinuationToken: "",
				}, nil).Once()
			},
			expectedError:  true,
			expectedCalled: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := CreateMockNatsMsg(tt.messageData)
			msg.reply = tt.replySubject

			handlerService := setupService()
			tt.setupMocks(handlerService, msg)

			// Test that the function doesn't panic
			assert.NotPanics(t, func() {
				err := handlerService.projectDeleteAllAccessHandler(msg)
				if tt.expectedError {
					assert.Error(t, err)
				} else {
					assert.NoError(t, err)
				}
			})

			// Verify mock expectations
			if tt.expectedCalled {
				msg.AssertExpectations(t)
			} else {
				// Ensure Respond was never called
				msg.AssertNotCalled(t, "Respond")
			}
		})
	}
}

// Helper function to create JSON or panic
func mustJSON(v any) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return data
}
