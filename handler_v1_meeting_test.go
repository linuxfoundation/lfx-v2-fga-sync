// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package main

import (
	"encoding/json"
	"testing"

	"github.com/linuxfoundation/lfx-v2-fga-sync/pkg/constants"
	openfga "github.com/openfga/go-sdk"
	. "github.com/openfga/go-sdk/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

// mustJSON is a helper function to marshal JSON that panics on error
func mustJSONV1(v interface{}) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return data
}

func TestBuildV1MeetingTuples(t *testing.T) {
	tests := []struct {
		name     string
		meeting  *v1MeetingStub
		expected int
	}{
		{
			name: "minimal v1 meeting",
			meeting: &v1MeetingStub{
				UID:        "test-uid",
				ProjectUID: "proj-123",
			},
			expected: 1, // project relation only
		},
		{
			name: "public v1 meeting with committees and hosts",
			meeting: &v1MeetingStub{
				UID:        "test-uid",
				Public:     true,
				ProjectUID: "proj-123",
				Committees: []string{"committee-1", "committee-2"},
				Hosts:      []string{"user1", "user2"},
			},
			expected: 6, // public + project + 2 committees + 2 hosts
		},
		{
			name: "private v1 meeting with single host",
			meeting: &v1MeetingStub{
				UID:        "test-uid",
				Public:     false,
				ProjectUID: "proj-123",
				Hosts:      []string{"host-user"},
			},
			expected: 2, // project + 1 host
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handlerService := &HandlerService{
				fgaService: FgaService{
					client:      &MockFgaClient{},
					cacheBucket: NewMockKeyValue(),
				},
			}

			object := constants.ObjectTypeV1Meeting + tt.meeting.UID
			tuples, err := handlerService.buildV1MeetingTuples(object, tt.meeting)

			assert.NoError(t, err)
			assert.Len(t, tuples, tt.expected)

			// Validate specific tuple types for the comprehensive test case
			if tt.name == "public v1 meeting with committees and hosts" {
				// Check that we have the expected relations
				foundPublic := false
				foundProject := false
				foundCommittees := 0
				foundHosts := 0

				for _, tuple := range tuples {
					if tuple.User == constants.UserWildcard && tuple.Relation == constants.RelationViewer {
						foundPublic = true
					}
					if tuple.User == constants.ObjectTypeProject+"proj-123" && tuple.Relation == constants.RelationProject {
						foundProject = true
					}
					if tuple.Relation == constants.RelationCommittee {
						foundCommittees++
					}
					if tuple.Relation == constants.RelationHost {
						foundHosts++
					}
				}

				assert.True(t, foundPublic, "should have public viewer relation")
				assert.True(t, foundProject, "should have project relation")
				assert.Equal(t, 2, foundCommittees, "should have 2 committee relations")
				assert.Equal(t, 2, foundHosts, "should have 2 host relations")
			}
		})
	}
}

func TestBuildV1PastMeetingTuples(t *testing.T) {
	tests := []struct {
		name        string
		pastMeeting *v1PastMeetingStub
		expected    int
	}{
		{
			name: "minimal v1 past meeting",
			pastMeeting: &v1PastMeetingStub{
				UID:          "past-uid",
				V1MeetingUID: "meeting-123",
				ProjectUID:   "proj-123",
			},
			expected: 2, // meeting relation + project relation
		},
		{
			name: "public v1 past meeting with committees",
			pastMeeting: &v1PastMeetingStub{
				UID:          "past-uid",
				V1MeetingUID: "meeting-123",
				Public:       true,
				ProjectUID:   "proj-123",
				Committees:   []string{"committee-1", "committee-2"},
			},
			expected: 5, // public + meeting + project + 2 committees
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handlerService := &HandlerService{
				fgaService: FgaService{
					client:      &MockFgaClient{},
					cacheBucket: NewMockKeyValue(),
				},
			}

			object := constants.ObjectTypeV1PastMeeting + tt.pastMeeting.UID
			tuples, err := handlerService.buildV1PastMeetingTuples(object, tt.pastMeeting)

			assert.NoError(t, err)
			assert.Len(t, tuples, tt.expected)

			// Validate specific tuple types for the comprehensive test case
			if tt.name == "public v1 past meeting with committees" {
				// Check that we have the expected relations
				foundPublic := false
				foundMeeting := false
				foundProject := false
				foundCommittees := 0

				for _, tuple := range tuples {
					if tuple.User == constants.UserWildcard && tuple.Relation == constants.RelationViewer {
						foundPublic = true
					}
					if tuple.User == constants.ObjectTypeV1Meeting+"meeting-123" && tuple.Relation == constants.RelationMeeting {
						foundMeeting = true
					}
					if tuple.User == constants.ObjectTypeProject+"proj-123" && tuple.Relation == constants.RelationProject {
						foundProject = true
					}
					if tuple.Relation == constants.RelationCommittee {
						foundCommittees++
					}
				}

				assert.True(t, foundPublic, "should have public viewer relation")
				assert.True(t, foundMeeting, "should have meeting relation")
				assert.True(t, foundProject, "should have project relation")
				assert.Equal(t, 2, foundCommittees, "should have 2 committee relations")
			}
		})
	}
}

func TestBuildV1PastMeetingArtifactTuples(t *testing.T) {
	participants := []V1PastMeetingParticipant{
		{Username: "host1", Host: true, IsInvited: true, IsAttended: true},
		{Username: "participant1", Host: false, IsInvited: true, IsAttended: true},
		{Username: "participant2", Host: false, IsInvited: false, IsAttended: true},
	}

	tests := []struct {
		name               string
		artifactVisibility string
		participants       []V1PastMeetingParticipant
		expected           int
		expectError        bool
	}{
		{
			name:               "public visibility",
			artifactVisibility: "public",
			participants:       participants,
			expected:           2, // past_meeting relation + public viewer
		},
		{
			name:               "meeting_hosts visibility",
			artifactVisibility: "meeting_hosts",
			participants:       participants,
			expected:           2, // past_meeting relation + 1 host viewer
		},
		{
			name:               "meeting_participants visibility",
			artifactVisibility: "meeting_participants",
			participants:       participants,
			expected:           4, // past_meeting relation + 3 participant viewers
		},
		{
			name:               "unknown visibility",
			artifactVisibility: "unknown",
			participants:       participants,
			expected:           0,
			expectError:        true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handlerService := &HandlerService{
				fgaService: FgaService{
					client:      &MockFgaClient{},
					cacheBucket: NewMockKeyValue(),
				},
			}

			object := constants.ObjectTypeV1PastMeetingRecording + "test-uid"
			tuples, err := handlerService.buildV1PastMeetingArtifactTuples(
				object,
				"past-meeting-uid",
				tt.artifactVisibility,
				tt.participants,
			)

			if tt.expectError {
				assert.Error(t, err)
				assert.Nil(t, tuples)
			} else {
				assert.NoError(t, err)
				assert.Len(t, tuples, tt.expected)
			}
		})
	}
}

func TestV1MeetingUpdateAccessHandler(t *testing.T) {
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
			name: "valid v1 meeting with all fields",
			messageData: mustJSONV1(v1MeetingStub{
				UID:        "meeting-123",
				Public:     true,
				ProjectUID: "project-456",
				Committees: []string{"committee1", "committee2"},
				Hosts:      []string{"host1", "host2"},
			}),
			replySubject: "reply.subject",
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				msg.On("Respond", []byte("OK")).Return(nil).Once()

				// Mock the Read operation for SyncObjectTuples
				service.fgaService.client.(*MockFgaClient).On("Read", mock.Anything, mock.MatchedBy(func(req ClientReadRequest) bool {
					return req.Object != nil && *req.Object == "v1_meeting:meeting-123"
				}), mock.Anything).Return(&ClientReadResponse{
					Tuples:            []openfga.Tuple{},
					ContinuationToken: "",
				}, nil).Once()

				// Mock the Write operation - expect 6 tuples:
				// 1 public viewer, 1 project relation, 2 committees, 2 hosts
				service.fgaService.client.(*MockFgaClient).On("Write", mock.Anything, mock.MatchedBy(func(req ClientWriteRequest) bool {
					return len(req.Writes) == 6 && len(req.Deletes) == 0
				})).Return(&ClientWriteResponse{}, nil).Once()
			},
			expectedError:  false,
			expectedReply:  "OK",
			expectedCalled: true,
		},
		{
			name:        "invalid JSON",
			messageData: []byte(`{invalid json}`),
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				// No mocks needed for parse error
			},
			expectedError:  true,
			expectedCalled: false,
		},
		{
			name: "missing project UID",
			messageData: mustJSONV1(v1MeetingStub{
				UID:    "meeting-123",
				Public: true,
			}),
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				// No mocks needed for validation error
			},
			expectedError:  true,
			expectedCalled: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockClient := &MockFgaClient{}
			service := &HandlerService{
				fgaService: FgaService{
					client:      mockClient,
					cacheBucket: NewMockKeyValue(),
				},
			}

			msg := &MockNatsMsg{
				data:    tt.messageData,
				reply:   tt.replySubject,
				subject: constants.V1MeetingUpdateAccessSubject,
			}

			if tt.setupMocks != nil {
				tt.setupMocks(service, msg)
			}

			err := service.v1MeetingUpdateAccessHandler(msg)

			if tt.expectedError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}

			mockClient.AssertExpectations(t)
			msg.AssertExpectations(t)
		})
	}
}

func TestV1PastMeetingUpdateAccessHandler(t *testing.T) {
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
			name: "valid v1 past meeting with all fields",
			messageData: mustJSONV1(v1PastMeetingStub{
				UID:          "past-meeting-123",
				V1MeetingUID: "meeting-456",
				Public:       false,
				ProjectUID:   "project-789",
				Committees:   []string{"committee1", "committee2"},
			}),
			replySubject: "reply.subject",
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				msg.On("Respond", []byte("OK")).Return(nil).Once()

				// Mock the Read operation for SyncObjectTuples
				service.fgaService.client.(*MockFgaClient).On("Read", mock.Anything, mock.MatchedBy(func(req ClientReadRequest) bool {
					return req.Object != nil && *req.Object == "v1_past_meeting:past-meeting-123"
				}), mock.Anything).Return(&ClientReadResponse{
					Tuples:            []openfga.Tuple{},
					ContinuationToken: "",
				}, nil).Once()

				// Mock the Write operation - expect 4 tuples:
				// 1 meeting relation, 1 project relation, 2 committees (no public viewer for private meeting)
				service.fgaService.client.(*MockFgaClient).On("Write", mock.Anything, mock.MatchedBy(func(req ClientWriteRequest) bool {
					return len(req.Writes) == 4 && len(req.Deletes) == 0
				})).Return(&ClientWriteResponse{}, nil).Once()
			},
			expectedError:  false,
			expectedReply:  "OK",
			expectedCalled: true,
		},
		{
			name:        "invalid JSON",
			messageData: []byte(`{invalid json}`),
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				// No mocks needed for parse error
			},
			expectedError:  true,
			expectedCalled: false,
		},
		{
			name: "missing project UID",
			messageData: mustJSONV1(v1PastMeetingStub{
				UID:          "past-meeting-123",
				V1MeetingUID: "meeting-456",
				Public:       true,
			}),
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				// No mocks needed for validation error
			},
			expectedError:  true,
			expectedCalled: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockClient := &MockFgaClient{}
			service := &HandlerService{
				fgaService: FgaService{
					client:      mockClient,
					cacheBucket: NewMockKeyValue(),
				},
			}

			msg := &MockNatsMsg{
				data:    tt.messageData,
				reply:   tt.replySubject,
				subject: constants.V1PastMeetingUpdateAccessSubject,
			}

			if tt.setupMocks != nil {
				tt.setupMocks(service, msg)
			}

			err := service.v1PastMeetingUpdateAccessHandler(msg)

			if tt.expectedError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}

			mockClient.AssertExpectations(t)
			msg.AssertExpectations(t)
		})
	}
}

func TestV1PastMeetingRecordingUpdateAccessHandler(t *testing.T) {
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
			name: "valid v1 recording with public visibility",
			messageData: mustJSONV1(V1PastMeetingRecordingAccessMessage{
				UID:                "recording-123",
				V1PastMeetingUID:   "past-meeting-456",
				ArtifactVisibility: "public",
				Participants: []V1PastMeetingParticipant{
					{Username: "user1", Host: true, IsInvited: true, IsAttended: true},
				},
			}),
			replySubject: "reply.subject",
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				msg.On("Respond", []byte("OK")).Return(nil).Once()

				// Mock the Read operation for SyncObjectTuples
				service.fgaService.client.(*MockFgaClient).On("Read", mock.Anything, mock.MatchedBy(func(req ClientReadRequest) bool {
					return req.Object != nil && *req.Object == "v1_past_meeting_recording:recording-123"
				}), mock.Anything).Return(&ClientReadResponse{
					Tuples:            []openfga.Tuple{},
					ContinuationToken: "",
				}, nil).Once()

				// Mock the Write operation - expect 2 tuples:
				// 1 past_meeting relation, 1 public viewer
				service.fgaService.client.(*MockFgaClient).On("Write", mock.Anything, mock.MatchedBy(func(req ClientWriteRequest) bool {
					return len(req.Writes) == 2 && len(req.Deletes) == 0
				})).Return(&ClientWriteResponse{}, nil).Once()
			},
			expectedError:  false,
			expectedReply:  "OK",
			expectedCalled: true,
		},
		{
			name:        "invalid JSON",
			messageData: []byte(`{invalid json}`),
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				// No mocks needed for parse error
			},
			expectedError:  true,
			expectedCalled: false,
		},
		{
			name: "missing v1 past meeting UID",
			messageData: mustJSONV1(V1PastMeetingRecordingAccessMessage{
				UID:                "recording-123",
				ArtifactVisibility: "public",
			}),
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				// No mocks needed for validation error
			},
			expectedError:  true,
			expectedCalled: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockClient := &MockFgaClient{}
			service := &HandlerService{
				fgaService: FgaService{
					client:      mockClient,
					cacheBucket: NewMockKeyValue(),
				},
			}

			msg := &MockNatsMsg{
				data:    tt.messageData,
				reply:   tt.replySubject,
				subject: constants.V1PastMeetingRecordingUpdateAccessSubject,
			}

			if tt.setupMocks != nil {
				tt.setupMocks(service, msg)
			}

			err := service.v1PastMeetingRecordingUpdateAccessHandler(msg)

			if tt.expectedError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}

			mockClient.AssertExpectations(t)
			msg.AssertExpectations(t)
		})
	}
}

func TestV1MeetingDeleteAllAccessHandler(t *testing.T) {
	mockClient := &MockFgaClient{}
	service := &HandlerService{
		fgaService: FgaService{
			client:      mockClient,
			cacheBucket: NewMockKeyValue(),
		},
	}

	msg := &MockNatsMsg{
		data:    []byte("test-uid"),
		reply:   "",
		subject: constants.V1MeetingDeleteAllAccessSubject,
	}

	// Mock the Read operation for SyncObjectTuples (should return existing tuples to delete)
	mockClient.On("Read", mock.Anything, mock.MatchedBy(func(req ClientReadRequest) bool {
		return req.Object != nil && *req.Object == "v1_meeting:test-uid"
	}), mock.Anything).Return(&ClientReadResponse{
		Tuples: []openfga.Tuple{
			{Key: openfga.TupleKey{Object: "v1_meeting:test-uid", Relation: "viewer", User: "user:test"}},
		},
		ContinuationToken: "",
	}, nil).Once()

	// Mock the Write operation to delete existing tuples
	mockClient.On("Write", mock.Anything, mock.MatchedBy(func(req ClientWriteRequest) bool {
		return len(req.Writes) == 0 && len(req.Deletes) == 1
	})).Return(&ClientWriteResponse{}, nil).Once()

	err := service.v1MeetingDeleteAllAccessHandler(msg)
	assert.NoError(t, err)

	mockClient.AssertExpectations(t)
}

func TestV1PastMeetingDeleteAllAccessHandler(t *testing.T) {
	mockClient := &MockFgaClient{}
	service := &HandlerService{
		fgaService: FgaService{
			client:      mockClient,
			cacheBucket: NewMockKeyValue(),
		},
	}

	msg := &MockNatsMsg{
		data:    []byte("test-uid"),
		reply:   "",
		subject: constants.V1PastMeetingDeleteAllAccessSubject,
	}

	// Mock the Read operation for SyncObjectTuples (should return existing tuples to delete)
	mockClient.On("Read", mock.Anything, mock.MatchedBy(func(req ClientReadRequest) bool {
		return req.Object != nil && *req.Object == "v1_past_meeting:test-uid"
	}), mock.Anything).Return(&ClientReadResponse{
		Tuples: []openfga.Tuple{
			{Key: openfga.TupleKey{Object: "v1_past_meeting:test-uid", Relation: "viewer", User: "user:test"}},
		},
		ContinuationToken: "",
	}, nil).Once()

	// Mock the Write operation to delete existing tuples
	mockClient.On("Write", mock.Anything, mock.MatchedBy(func(req ClientWriteRequest) bool {
		return len(req.Writes) == 0 && len(req.Deletes) == 1
	})).Return(&ClientWriteResponse{}, nil).Once()

	err := service.v1PastMeetingDeleteAllAccessHandler(msg)
	assert.NoError(t, err)

	mockClient.AssertExpectations(t)
}

// TestV1MeetingRegistrantPutHandler tests the v1MeetingRegistrantPutHandler function
func TestV1MeetingRegistrantPutHandler(t *testing.T) {
	tests := []struct {
		name           string
		messageData    []byte
		replySubject   string
		setupMocks     func(*HandlerService, *MockNatsMsg)
		expectedError  bool
		expectedCalled bool
	}{
		{
			name: "put v1 participant (new registrant)",
			messageData: mustJSONV1(v1RegistrantStub{
				ID:        "registrant-123",
				Username:  "user-123",
				MeetingID: "meeting-456",
				Host:      false,
			}),
			replySubject: "reply.subject",
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				msg.On("Respond", []byte("OK")).Return(nil).Once()

				// Mock the Read operation to check existing relations (return empty - new registrant)
				service.fgaService.client.(*MockFgaClient).On("Read", mock.Anything, mock.MatchedBy(func(req ClientReadRequest) bool {
					return req.Object != nil && *req.Object == "v1_meeting:meeting-456"
				}), mock.Anything).Return(&ClientReadResponse{
					Tuples:            []openfga.Tuple{},
					ContinuationToken: "",
				}, nil).Once()

				// Mock the Write operation to add new participant relation
				service.fgaService.client.(*MockFgaClient).On("Write", mock.Anything, mock.MatchedBy(func(req ClientWriteRequest) bool {
					return len(req.Writes) == 1 &&
						req.Writes[0].User == "user:user-123" &&
						req.Writes[0].Relation == "participant" &&
						req.Writes[0].Object == "v1_meeting:meeting-456"
				})).Return(&ClientWriteResponse{}, nil).Once()
			},
			expectedError: false,
		},
		{
			name: "put v1 host (new registrant)",
			messageData: mustJSONV1(v1RegistrantStub{
				ID:        "registrant-456",
				Username:  "host-123",
				MeetingID: "meeting-456",
				Host:      true,
			}),
			replySubject: "",
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				// No reply expected

				// Mock the Read operation to check existing relations (return empty - new registrant)
				service.fgaService.client.(*MockFgaClient).On("Read", mock.Anything, mock.MatchedBy(func(req ClientReadRequest) bool {
					return req.Object != nil && *req.Object == "v1_meeting:meeting-456"
				}), mock.Anything).Return(&ClientReadResponse{
					Tuples:            []openfga.Tuple{},
					ContinuationToken: "",
				}, nil).Once()

				// Mock the Write operation to add new host relation
				service.fgaService.client.(*MockFgaClient).On("Write", mock.Anything, mock.MatchedBy(func(req ClientWriteRequest) bool {
					return len(req.Writes) == 1 &&
						req.Writes[0].User == "user:host-123" &&
						req.Writes[0].Relation == "host" &&
						req.Writes[0].Object == "v1_meeting:meeting-456"
				})).Return(&ClientWriteResponse{}, nil).Once()
			},
			expectedError: false,
		},
		{
			name: "put v1 participant to host (role change)",
			messageData: mustJSONV1(v1RegistrantStub{
				ID:        "registrant-789",
				Username:  "user-123",
				MeetingID: "meeting-456",
				Host:      true,
			}),
			replySubject: "reply.subject",
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				msg.On("Respond", []byte("OK")).Return(nil).Once()

				// Mock the Read operation to return existing participant relation
				service.fgaService.client.(*MockFgaClient).On("Read", mock.Anything, mock.MatchedBy(func(req ClientReadRequest) bool {
					return req.Object != nil && *req.Object == "v1_meeting:meeting-456"
				}), mock.Anything).Return(&ClientReadResponse{
					Tuples: []openfga.Tuple{
						{Key: openfga.TupleKey{User: "user:user-123", Relation: "participant", Object: "v1_meeting:meeting-456"}},
					},
					ContinuationToken: "",
				}, nil).Once()

				// Mock the Write operation to delete old relation and add new host relation
				service.fgaService.client.(*MockFgaClient).On("Write", mock.Anything, mock.MatchedBy(func(req ClientWriteRequest) bool {
					return len(req.Deletes) == 1 &&
						req.Deletes[0].User == "user:user-123" &&
						req.Deletes[0].Relation == "participant" &&
						req.Deletes[0].Object == "v1_meeting:meeting-456"
				})).Return(&ClientWriteResponse{}, nil).Once()

				// Mock the Write operation to add new host relation
				service.fgaService.client.(*MockFgaClient).On("Write", mock.Anything, mock.MatchedBy(func(req ClientWriteRequest) bool {
					return len(req.Writes) == 1 &&
						req.Writes[0].User == "user:user-123" &&
						req.Writes[0].Relation == "host" &&
						req.Writes[0].Object == "v1_meeting:meeting-456"
				})).Return(&ClientWriteResponse{}, nil).Once()
			},
			expectedError: false,
		},
		{
			name: "put v1 host - already exists (no changes)",
			messageData: mustJSONV1(v1RegistrantStub{
				ID:        "registrant-000",
				Username:  "host-123",
				MeetingID: "meeting-456",
				Host:      true,
			}),
			replySubject: "",
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				// No reply expected

				// Mock the Read operation to return existing host relation
				service.fgaService.client.(*MockFgaClient).On("Read", mock.Anything, mock.MatchedBy(func(req ClientReadRequest) bool {
					return req.Object != nil && *req.Object == "v1_meeting:meeting-456"
				}), mock.Anything).Return(&ClientReadResponse{
					Tuples: []openfga.Tuple{
						{Key: openfga.TupleKey{User: "user:host-123", Relation: "host", Object: "v1_meeting:meeting-456"}},
					},
					ContinuationToken: "",
				}, nil).Once()

				// No Write operations should be called since relation already exists
			},
			expectedError: false,
		},
		{
			name:        "invalid JSON",
			messageData: []byte("invalid json"),
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				// No operations expected due to parse error
			},
			expectedError: true,
		},
		{
			name: "missing v1 username",
			messageData: mustJSONV1(v1RegistrantStub{
				ID:        "registrant-111",
				MeetingID: "meeting-456",
				Host:      false,
			}),
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				// No operations expected due to validation error
			},
			expectedError: true,
		},
		{
			name: "missing v1 meeting ID",
			messageData: mustJSONV1(v1RegistrantStub{
				ID:       "registrant-222",
				Username: "user-123",
				Host:     false,
			}),
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				// No operations expected due to validation error
			},
			expectedError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := CreateMockNatsMsg(tt.messageData)
			msg.reply = tt.replySubject

			handlerService := setupService()
			if tt.setupMocks != nil {
				tt.setupMocks(handlerService, msg)
			}

			assert.NotPanics(t, func() {
				err := handlerService.v1MeetingRegistrantPutHandler(msg)
				if tt.expectedError {
					assert.Error(t, err)
				} else {
					assert.NoError(t, err)
				}
			})

			msg.AssertExpectations(t)
		})
	}
}

// TestV1MeetingRegistrantRemoveHandler tests the v1MeetingRegistrantRemoveHandler function
func TestV1MeetingRegistrantRemoveHandler(t *testing.T) {
	tests := []struct {
		name           string
		messageData    []byte
		replySubject   string
		setupMocks     func(*HandlerService, *MockNatsMsg)
		expectedError  bool
		expectedCalled bool
	}{
		{
			name: "remove v1 participant",
			messageData: mustJSONV1(v1RegistrantStub{
				ID:        "registrant-333",
				Username:  "user-123",
				MeetingID: "meeting-456",
				Host:      false,
			}),
			replySubject: "reply.subject",
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				msg.On("Respond", []byte("OK")).Return(nil).Once()

				// Mock the Write operation to delete participant relation
				service.fgaService.client.(*MockFgaClient).On("Write", mock.Anything, mock.MatchedBy(func(req ClientWriteRequest) bool {
					return len(req.Deletes) == 1 &&
						req.Deletes[0].User == "user:user-123" &&
						req.Deletes[0].Relation == "participant" &&
						req.Deletes[0].Object == "v1_meeting:meeting-456"
				})).Return(&ClientWriteResponse{}, nil).Once()
			},
			expectedError: false,
		},
		{
			name: "remove v1 host",
			messageData: mustJSONV1(v1RegistrantStub{
				ID:        "registrant-444",
				Username:  "host-123",
				MeetingID: "meeting-456",
				Host:      true,
			}),
			replySubject: "",
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				// No reply expected

				// Mock the Write operation to delete host relation
				service.fgaService.client.(*MockFgaClient).On("Write", mock.Anything, mock.MatchedBy(func(req ClientWriteRequest) bool {
					return len(req.Deletes) == 1 &&
						req.Deletes[0].User == "user:host-123" &&
						req.Deletes[0].Relation == "host" &&
						req.Deletes[0].Object == "v1_meeting:meeting-456"
				})).Return(&ClientWriteResponse{}, nil).Once()
			},
			expectedError: false,
		},
		{
			name:        "invalid JSON",
			messageData: []byte("invalid json"),
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				// No operations expected due to parse error
			},
			expectedError: true,
		},
		{
			name: "missing v1 username",
			messageData: mustJSONV1(v1RegistrantStub{
				ID:        "registrant-555",
				MeetingID: "meeting-456",
				Host:      false,
			}),
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				// No operations expected due to validation error
			},
			expectedError: true,
		},
		{
			name: "missing v1 meeting ID",
			messageData: mustJSONV1(v1RegistrantStub{
				ID:       "registrant-666",
				Username: "user-123",
				Host:     false,
			}),
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				// No operations expected due to validation error
			},
			expectedError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := CreateMockNatsMsg(tt.messageData)
			msg.reply = tt.replySubject

			handlerService := setupService()
			if tt.setupMocks != nil {
				tt.setupMocks(handlerService, msg)
			}

			assert.NotPanics(t, func() {
				err := handlerService.v1MeetingRegistrantRemoveHandler(msg)
				if tt.expectedError {
					assert.Error(t, err)
				} else {
					assert.NoError(t, err)
				}
			})

			msg.AssertExpectations(t)
		})
	}
}
