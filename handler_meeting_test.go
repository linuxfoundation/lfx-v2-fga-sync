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

// TestMeetingUpdateAccessHandler tests the meetingUpdateAccessHandler function
func TestMeetingUpdateAccessHandler(t *testing.T) {
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
			name: "valid meeting with all fields",
			messageData: mustJSON(meetingStub{
				UID:        "meeting-123",
				Public:     true,
				ProjectUID: "project-456",
				Organizers: []string{"organizer1", "organizer2"},
				Committees: []string{"committee1", "committee2"},
			}),
			replySubject: "reply.subject",
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				msg.On("Respond", []byte("OK")).Return(nil).Once()

				// Mock the Read operation for SyncObjectTuples
				service.fgaService.client.(*MockFgaClient).On("Read", mock.Anything, mock.MatchedBy(func(req ClientReadRequest) bool {
					return req.Object != nil && *req.Object == "meeting:meeting-123"
				}), mock.Anything).Return(&ClientReadResponse{
					Tuples:            []openfga.Tuple{},
					ContinuationToken: "",
				}, nil).Once()

				// Mock the Write operation - expect 6 tuples:
				// 1 public viewer, 1 project relation, 2 committees, 2 meeting organizers
				service.fgaService.client.(*MockFgaClient).On("Write", mock.Anything, mock.MatchedBy(func(req ClientWriteRequest) bool {
					return len(req.Writes) == 6 && len(req.Deletes) == 0
				})).Return(&ClientWriteResponse{}, nil).Once()

				// Mock cache operations
				service.fgaService.cacheBucket.(*MockKeyValue).On("Put", mock.Anything, "inv", mock.Anything).Return(uint64(1), nil).Twice()
			},
			expectedError:  false,
			expectedCalled: true,
		},
		{
			name: "private meeting with no committees",
			messageData: mustJSON(meetingStub{
				UID:        "private-meeting",
				Public:     false,
				ProjectUID: "project-123",
				Organizers: []string{"organizer1"},
				Committees: []string{},
			}),
			replySubject: "",
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				// No reply expected when replySubject is empty

				// Mock GetTuplesByRelation call - return empty
				service.fgaService.client.(*MockFgaClient).On("Read", mock.Anything, mock.MatchedBy(func(req ClientReadRequest) bool {
					return req.Object != nil && *req.Object == "project:project-123"
				}), mock.Anything).Return(&ClientReadResponse{
					Tuples:            []openfga.Tuple{},
					ContinuationToken: "",
				}, nil).Once()

				// Mock the Read operation for SyncObjectTuples
				service.fgaService.client.(*MockFgaClient).On("Read", mock.Anything, mock.MatchedBy(func(req ClientReadRequest) bool {
					return req.Object != nil && *req.Object == "meeting:private-meeting"
				}), mock.Anything).Return(&ClientReadResponse{
					Tuples:            []openfga.Tuple{},
					ContinuationToken: "",
				}, nil).Once()

				// Mock the Write operation - expect 2 tuples: 1 project relation, 1 meeting organizer
				service.fgaService.client.(*MockFgaClient).On("Write", mock.Anything, mock.MatchedBy(func(req ClientWriteRequest) bool {
					return len(req.Writes) == 2 && len(req.Deletes) == 0
				})).Return(&ClientWriteResponse{}, nil).Once()

				// Mock cache operations
				service.fgaService.cacheBucket.(*MockKeyValue).On("Put", mock.Anything, "inv", mock.Anything).Return(uint64(1), nil).Twice()
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
			name:         "missing project UID",
			messageData:  mustJSON(meetingStub{UID: "meeting-123"}),
			replySubject: "reply.subject",
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				// No mocks needed - should fail at project UID validation
			},
			expectedError:  true,
			expectedCalled: false,
		},
		{
			name: "GetTuplesByRelation fails - should continue",
			messageData: mustJSON(meetingStub{
				UID:        "meeting-error",
				ProjectUID: "project-456",
				Organizers: []string{"organizer1"},
			}),
			replySubject: "",
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				// Mock GetTuplesByRelation call to fail
				service.fgaService.client.(*MockFgaClient).On("Read", mock.Anything, mock.MatchedBy(func(req ClientReadRequest) bool {
					return req.Object != nil && *req.Object == "project:project-456"
				}), mock.Anything).Return((*ClientReadResponse)(nil), assert.AnError).Once()

				// Mock the Read operation for SyncObjectTuples
				service.fgaService.client.(*MockFgaClient).On("Read", mock.Anything, mock.MatchedBy(func(req ClientReadRequest) bool {
					return req.Object != nil && *req.Object == "meeting:meeting-error"
				}), mock.Anything).Return(&ClientReadResponse{
					Tuples:            []openfga.Tuple{},
					ContinuationToken: "",
				}, nil).Once()

				// Mock the Write operation - expect 2 tuples: 1 project relation, 1 meeting organizer
				service.fgaService.client.(*MockFgaClient).On("Write", mock.Anything, mock.MatchedBy(func(req ClientWriteRequest) bool {
					return len(req.Writes) == 2 && len(req.Deletes) == 0
				})).Return(&ClientWriteResponse{}, nil).Once()

				// Mock cache operations
				service.fgaService.cacheBucket.(*MockKeyValue).On("Put", mock.Anything, "inv", mock.Anything).Return(uint64(1), nil).Twice()
			},
			expectedError:  false,
			expectedCalled: false,
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
				err := handlerService.meetingUpdateAccessHandler(msg)
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

// TestMeetingDeleteAllAccessHandler tests the meetingDeleteAllAccessHandler function
func TestMeetingDeleteAllAccessHandler(t *testing.T) {
	tests := []struct {
		name           string
		messageData    []byte
		replySubject   string
		setupMocks     func(*HandlerService, *MockNatsMsg)
		expectedError  bool
		expectedCalled bool
	}{
		{
			name:         "valid meeting UID",
			messageData:  []byte("meeting-123"),
			replySubject: "reply.subject",
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				msg.On("Respond", []byte("OK")).Return(nil).Once()

				// Mock the Read operation to return some existing tuples
				service.fgaService.client.(*MockFgaClient).On("Read", mock.Anything, mock.MatchedBy(func(req ClientReadRequest) bool {
					return req.Object != nil && *req.Object == "meeting:meeting-123"
				}), mock.Anything).Return(&ClientReadResponse{
					Tuples: []openfga.Tuple{
						{Key: openfga.TupleKey{User: "user:organizer1", Relation: "organizer", Object: "meeting:meeting-123"}},
						{Key: openfga.TupleKey{User: "committee:committee1", Relation: "committee", Object: "meeting:meeting-123"}},
					},
					ContinuationToken: "",
				}, nil).Once()

				// Mock the Write operation for deletion (only deletes)
				service.fgaService.client.(*MockFgaClient).On("Write", mock.Anything, mock.MatchedBy(func(req ClientWriteRequest) bool {
					return len(req.Writes) == 0 && len(req.Deletes) == 2
				})).Return(&ClientWriteResponse{}, nil).Once()

				// Mock cache operations
				service.fgaService.cacheBucket.(*MockKeyValue).On("Put", mock.Anything, "inv", mock.Anything).Return(uint64(1), nil).Once()
			},
			expectedError:  false,
			expectedCalled: true,
		},
		{
			name:         "empty meeting UID",
			messageData:  []byte(""),
			replySubject: "reply.subject",
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				// No mocks needed - should fail at UID validation
			},
			expectedError:  true,
			expectedCalled: false,
		},
		{
			name:         "serialized JSON payload (should fail)",
			messageData:  []byte(`{"uid": "meeting-123"}`),
			replySubject: "reply.subject",
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				// No mocks needed - should fail at payload validation
			},
			expectedError:  true,
			expectedCalled: false,
		},
		{
			name:         "array payload (should fail)",
			messageData:  []byte(`["meeting-123"]`),
			replySubject: "reply.subject",
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				// No mocks needed - should fail at payload validation
			},
			expectedError:  true,
			expectedCalled: false,
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
				err := handlerService.meetingDeleteAllAccessHandler(msg)
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
				msg.AssertNotCalled(t, "Respond")
			}
		})
	}
}

// TestMeetingRegistrantPutHandler tests the meetingRegistrantPutHandler function
func TestMeetingRegistrantPutHandler(t *testing.T) {
	tests := []struct {
		name           string
		messageData    []byte
		replySubject   string
		setupMocks     func(*HandlerService, *MockNatsMsg)
		expectedError  bool
		expectedCalled bool
	}{
		{
			name: "put participant (new registrant)",
			messageData: mustJSON(registrantStub{
				Username:   "user-123",
				MeetingUID: "meeting-456",
				Host:       false,
			}),
			replySubject: "reply.subject",
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				msg.On("Respond", []byte("OK")).Return(nil).Once()

				// Mock the Read operation to check existing relations (return empty - new registrant)
				service.fgaService.client.(*MockFgaClient).On("Read", mock.Anything, mock.MatchedBy(func(req ClientReadRequest) bool {
					return req.Object != nil && *req.Object == "meeting:meeting-456"
				}), mock.Anything).Return(&ClientReadResponse{
					Tuples:            []openfga.Tuple{},
					ContinuationToken: "",
				}, nil).Once()

				// Mock the WriteAndDeleteTuples operation for participant relation
				service.fgaService.client.(*MockFgaClient).On("Write", mock.Anything, mock.MatchedBy(func(req ClientWriteRequest) bool {
					return len(req.Writes) == 1 && len(req.Deletes) == 0 &&
						req.Writes[0].User == "user:user-123" &&
						req.Writes[0].Relation == "participant" &&
						req.Writes[0].Object == "meeting:meeting-456"
				})).Return(&ClientWriteResponse{}, nil).Once()

				// Mock cache operations
				service.fgaService.cacheBucket.(*MockKeyValue).On("Put", mock.Anything, "inv", mock.Anything).Return(uint64(1), nil).Once()
			},
			expectedError:  false,
			expectedCalled: true,
		},
		{
			name: "put host (new registrant)",
			messageData: mustJSON(registrantStub{
				Username:   "host-123",
				MeetingUID: "meeting-456",
				Host:       true,
			}),
			replySubject: "",
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				// No reply expected

				// Mock the Read operation to check existing relations (return empty - new registrant)
				service.fgaService.client.(*MockFgaClient).On("Read", mock.Anything, mock.MatchedBy(func(req ClientReadRequest) bool {
					return req.Object != nil && *req.Object == "meeting:meeting-456"
				}), mock.Anything).Return(&ClientReadResponse{
					Tuples:            []openfga.Tuple{},
					ContinuationToken: "",
				}, nil).Once()

				// Mock the WriteAndDeleteTuples operation for host relation
				service.fgaService.client.(*MockFgaClient).On("Write", mock.Anything, mock.MatchedBy(func(req ClientWriteRequest) bool {
					return len(req.Writes) == 1 && len(req.Deletes) == 0 &&
						req.Writes[0].User == "user:host-123" &&
						req.Writes[0].Relation == "host" &&
						req.Writes[0].Object == "meeting:meeting-456"
				})).Return(&ClientWriteResponse{}, nil).Once()

				// Mock cache operations
				service.fgaService.cacheBucket.(*MockKeyValue).On("Put", mock.Anything, "inv", mock.Anything).Return(uint64(1), nil).Once()
			},
			expectedError:  false,
			expectedCalled: false,
		},
		{
			name: "put participant to host (role change)",
			messageData: mustJSON(registrantStub{
				Username:   "user-123",
				MeetingUID: "meeting-456",
				Host:       true,
			}),
			replySubject: "reply.subject",
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				msg.On("Respond", []byte("OK")).Return(nil).Once()

				// Mock the Read operation to return existing participant relation
				service.fgaService.client.(*MockFgaClient).On("Read", mock.Anything, mock.MatchedBy(func(req ClientReadRequest) bool {
					return req.Object != nil && *req.Object == "meeting:meeting-456"
				}), mock.Anything).Return(&ClientReadResponse{
					Tuples: []openfga.Tuple{
						{Key: openfga.TupleKey{User: "user:user-123", Relation: "participant", Object: "meeting:meeting-456"}},
					},
					ContinuationToken: "",
				}, nil).Once()

				// Mock the WriteAndDeleteTuples operation (delete participant, add host)
				service.fgaService.client.(*MockFgaClient).On("Write", mock.Anything, mock.MatchedBy(func(req ClientWriteRequest) bool {
					return len(req.Writes) == 1 && len(req.Deletes) == 1 &&
						req.Writes[0].User == "user:user-123" &&
						req.Writes[0].Relation == "host" &&
						req.Writes[0].Object == "meeting:meeting-456" &&
						req.Deletes[0].User == "user:user-123" &&
						req.Deletes[0].Relation == "participant" &&
						req.Deletes[0].Object == "meeting:meeting-456"
				})).Return(&ClientWriteResponse{}, nil).Once()

				// Mock cache operations
				service.fgaService.cacheBucket.(*MockKeyValue).On("Put", mock.Anything, "inv", mock.Anything).Return(uint64(1), nil).Once()
			},
			expectedError:  false,
			expectedCalled: true,
		},
		{
			name: "put host - already exists (no changes)",
			messageData: mustJSON(registrantStub{
				Username:   "host-123",
				MeetingUID: "meeting-456",
				Host:       true,
			}),
			replySubject: "",
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				// No reply expected

				// Mock the Read operation to return existing host relation
				service.fgaService.client.(*MockFgaClient).On("Read", mock.Anything, mock.MatchedBy(func(req ClientReadRequest) bool {
					return req.Object != nil && *req.Object == "meeting:meeting-456"
				}), mock.Anything).Return(&ClientReadResponse{
					Tuples: []openfga.Tuple{
						{Key: openfga.TupleKey{User: "user:host-123", Relation: "host", Object: "meeting:meeting-456"}},
					},
					ContinuationToken: "",
				}, nil).Once()

				// No WriteAndDeleteTuples call expected since no changes needed
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
			name:         "missing registrant LFID",
			messageData:  mustJSON(registrantStub{MeetingUID: "meeting-456"}),
			replySubject: "reply.subject",
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				// No mocks needed - should fail at LFID validation
			},
			expectedError:  true,
			expectedCalled: false,
		},
		{
			name:         "missing meeting UID",
			messageData:  mustJSON(registrantStub{Username: "user-123"}),
			replySubject: "reply.subject",
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				// No mocks needed - should fail at meeting UID validation
			},
			expectedError:  true,
			expectedCalled: false,
		},
		{
			name: "read operation fails",
			messageData: mustJSON(registrantStub{
				Username:   "user-123",
				MeetingUID: "meeting-456",
				Host:       false,
			}),
			replySubject: "",
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				// Mock the Read operation to fail
				service.fgaService.client.(*MockFgaClient).On("Read", mock.Anything, mock.MatchedBy(func(req ClientReadRequest) bool {
					return req.Object != nil && *req.Object == "meeting:meeting-456"
				}), mock.Anything).Return((*ClientReadResponse)(nil), assert.AnError).Once()
			},
			expectedError:  true,
			expectedCalled: false,
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
				err := handlerService.meetingRegistrantPutHandler(msg)
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
				msg.AssertNotCalled(t, "Respond")
			}
		})
	}
}

// TestMeetingRegistrantRemoveHandler tests the meetingRegistrantRemoveHandler function
func TestMeetingRegistrantRemoveHandler(t *testing.T) {
	tests := []struct {
		name           string
		messageData    []byte
		replySubject   string
		setupMocks     func(*HandlerService, *MockNatsMsg)
		expectedError  bool
		expectedCalled bool
	}{
		{
			name: "remove participant",
			messageData: mustJSON(registrantStub{
				Username:   "user-123",
				MeetingUID: "meeting-456",
				Host:       false,
			}),
			replySubject: "reply.subject",
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				msg.On("Respond", []byte("OK")).Return(nil).Once()

				// Mock the Write operation for deleting participant relation
				service.fgaService.client.(*MockFgaClient).On("Write", mock.Anything, mock.MatchedBy(func(req ClientWriteRequest) bool {
					return len(req.Writes) == 0 && len(req.Deletes) == 1 &&
						req.Deletes[0].User == "user:user-123" &&
						req.Deletes[0].Relation == "participant" &&
						req.Deletes[0].Object == "meeting:meeting-456"
				})).Return(&ClientWriteResponse{}, nil).Once()

				// Mock cache operations
				service.fgaService.cacheBucket.(*MockKeyValue).On("Put", mock.Anything, "inv", mock.Anything).Return(uint64(1), nil).Once()
			},
			expectedError:  false,
			expectedCalled: true,
		},
		{
			name: "remove host",
			messageData: mustJSON(registrantStub{
				Username:   "host-123",
				MeetingUID: "meeting-456",
				Host:       true,
			}),
			replySubject: "",
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				// No reply expected

				// Mock the Write operation for deleting host relation
				service.fgaService.client.(*MockFgaClient).On("Write", mock.Anything, mock.MatchedBy(func(req ClientWriteRequest) bool {
					return len(req.Writes) == 0 && len(req.Deletes) == 1 &&
						req.Deletes[0].User == "user:host-123" &&
						req.Deletes[0].Relation == "host" &&
						req.Deletes[0].Object == "meeting:meeting-456"
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
			name:         "missing registrant UID",
			messageData:  mustJSON(registrantStub{MeetingUID: "meeting-456"}),
			replySubject: "reply.subject",
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				// No mocks needed - should fail at UID validation
			},
			expectedError:  true,
			expectedCalled: false,
		},
		{
			name:         "missing meeting UID",
			messageData:  mustJSON(registrantStub{UID: "user-123"}),
			replySubject: "reply.subject",
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				// No mocks needed - should fail at meeting UID validation
			},
			expectedError:  true,
			expectedCalled: false,
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
				err := handlerService.meetingRegistrantRemoveHandler(msg)
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
				msg.AssertNotCalled(t, "Respond")
			}
		})
	}
}
