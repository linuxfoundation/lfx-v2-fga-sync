// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package main

import (
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/nats-io/nats.go/jetstream"
	openfga "github.com/openfga/go-sdk"
	"github.com/openfga/go-sdk/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

func init() {
	// Initialize logger for all tests
	if logger == nil {
		logOptions := &slog.HandlerOptions{}

		// Optional debug logging.
		if os.Getenv("DEBUG") != "" {
			logOptions.Level = slog.LevelDebug
			logOptions.AddSource = true
		}

		logger = slog.New(slog.NewTextHandler(os.Stdout, logOptions))
		slog.SetDefault(logger)
	}
}

// setupService creates a new ProjectsService with mocked external service APIs.
func setupService() *HandlerService {
	if os.Getenv("DEBUG") == "true" {
		slog.SetLogLoggerLevel(slog.LevelDebug)
	}
	service := &HandlerService{
		fgaService: FgaService{
			client:      &MockFgaClient{},
			cacheBucket: NewMockKeyValue(),
		},
	}

	return service
}

// TestAccessCheckHandler tests the [accessCheckHandler] function.
func TestAccessCheckHandler(t *testing.T) {
	tests := []struct {
		name           string
		messageData    []byte
		replySubject   string
		setupMocks     func(HandlerService, *MockNatsMsg)
		expectedError  bool
		expectedReply  string
		expectedCalled bool
	}{
		{
			name:         "valid single check request",
			messageData:  []byte("project:123#writer@user:456"),
			replySubject: "reply.subject",
			setupMocks: func(service HandlerService, msg *MockNatsMsg) {
				msg.On("Respond", mock.MatchedBy(func(data []byte) bool {
					// Expect a JSON response with check results
					response := string(data)
					return strings.Contains(response, "project:123#writer@user:456\ttrue")
				})).Return(nil).Once()
				// Create the result map for single request
				resultMap := make(map[string]openfga.BatchCheckSingleResult)
				resultMap["1"] = openfga.BatchCheckSingleResult{
					Allowed: openfga.PtrBool(true),
				}
				service.fgaService.client.(*MockFgaClient).On("BatchCheck", mock.Anything, mock.Anything).Return(&openfga.BatchCheckResponse{
					Result: &resultMap,
				}, nil)
				service.fgaService.cacheBucket.(*MockKeyValue).On("PutString", mock.Anything, mock.Anything, mock.Anything).Return(uint64(0), nil)
			},
			expectedError:  false,
			expectedCalled: true,
		},
		{
			name:         "valid multiple check requests",
			messageData:  []byte("project:123#writer@user:456\nproject:789#viewer@user:456"),
			replySubject: "reply.subject",
			setupMocks: func(service HandlerService, msg *MockNatsMsg) {
				msg.On("Respond", mock.MatchedBy(func(data []byte) bool {
					// Expect response with both check results
					response := string(data)
					return strings.Contains(response, "project:123#writer@user:456\ttrue") &&
						strings.Contains(response, "project:789#viewer@user:456\ttrue")
				})).Return(nil).Once()
				// Create the result map for multiple requests
				resultMap := make(map[string]openfga.BatchCheckSingleResult, 2)
				resultMap["1"] = openfga.BatchCheckSingleResult{
					Allowed: openfga.PtrBool(true),
				}
				resultMap["2"] = openfga.BatchCheckSingleResult{
					Allowed: openfga.PtrBool(true),
				}
				service.fgaService.client.(*MockFgaClient).On("BatchCheck", mock.Anything, mock.Anything).Return(&openfga.BatchCheckResponse{
					Result: &resultMap,
				}, nil)
				// Mock cache operations
				service.fgaService.cacheBucket.(*MockKeyValue).On("Get", mock.Anything, "inv").Return(nil, jetstream.ErrKeyNotFound)
				service.fgaService.cacheBucket.(*MockKeyValue).On("Get", mock.Anything, mock.AnythingOfType("string")).Return(nil, jetstream.ErrKeyNotFound)
				service.fgaService.cacheBucket.(*MockKeyValue).On("Put", mock.Anything, mock.Anything, mock.Anything).Return(uint64(0), nil)
			},
			expectedError:  false,
			expectedCalled: true,
		},
		{
			name:         "invalid check request format",
			messageData:  []byte("invalid-format"),
			replySubject: "reply.subject",
			setupMocks: func(service HandlerService, msg *MockNatsMsg) {
				msg.On("Respond", []byte("failed to extract check requests")).Return(nil).Once()
			},
			expectedReply:  "failed to extract check requests",
			expectedError:  true,
			expectedCalled: true,
		},
		{
			name:         "empty message data",
			messageData:  []byte(""),
			replySubject: "reply.subject",
			setupMocks: func(service HandlerService, msg *MockNatsMsg) {
				msg.On("Respond", []byte("no check requests found")).Return(nil).Once()
			},
			expectedReply:  "no check requests found",
			expectedError:  false,
			expectedCalled: true,
		},
		{
			name:         "no reply subject - should not respond",
			messageData:  []byte("project:123#writer@user:456"),
			replySubject: "",
			setupMocks: func(service HandlerService, msg *MockNatsMsg) {
				// No Respond call expected
				resultMap := make(map[string]openfga.BatchCheckSingleResult)
				resultMap["1"] = openfga.BatchCheckSingleResult{
					Allowed: openfga.PtrBool(true),
				}
				service.fgaService.client.(*MockFgaClient).On("BatchCheck", mock.Anything, mock.Anything).Return(&openfga.BatchCheckResponse{
					Result: &resultMap,
				}, nil)
			},
			expectedError:  false,
			expectedCalled: false,
		},
		{
			name:         "respond error handling",
			messageData:  []byte("invalid-format"),
			replySubject: "reply.subject",
			setupMocks: func(service HandlerService, msg *MockNatsMsg) {
				msg.On("Respond", []byte("failed to extract check requests")).Return(assert.AnError).Once()
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
			tt.setupMocks(*handlerService, msg)

			// Test that the function doesn't panic
			assert.NotPanics(t, func() {
				err := handlerService.accessCheckHandler(msg)
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

// TestProcessStandardAccessUpdate tests the processStandardAccessUpdate function with intermediate and hard scenarios
func TestProcessStandardAccessUpdate(t *testing.T) {
	tests := []struct {
		name           string
		obj            *standardAccessStub
		replySubject   string
		setupMocks     func(*HandlerService, *MockNatsMsg)
		expectedError  bool
		expectedCalled bool
	}{
		// Intermediate Tests
		{
			name: "basic valid object with public access",
			obj: &standardAccessStub{
				UID:        "test-123",
				ObjectType: "committee",
				Public:     true,
				Relations:  map[string][]string{"writer": {"user1", "user2"}},
				References: map[string][]string{"parent": {"parent-123"}},
			},
			replySubject: "reply.subject",
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				msg.On("Respond", []byte("OK")).Return(nil).Once()
				service.fgaService.client.(*MockFgaClient).On("Write", mock.Anything, mock.MatchedBy(func(req client.ClientWriteRequest) bool {
					// Should have: public viewer, parent relation, 2 writers = 4 tuples
					return len(req.Writes) == 4 && len(req.Deletes) == 0
				})).Return(&client.ClientWriteResponse{}, nil)
				service.fgaService.client.(*MockFgaClient).On("Read", mock.Anything, mock.Anything, mock.Anything).Return(&client.ClientReadResponse{}, nil)
			},
			expectedError:  false,
			expectedCalled: true,
		},
		{
			name: "object with multiple relations and references",
			obj: &standardAccessStub{
				UID:        "complex-456",
				ObjectType: "groupsio_service",
				Public:     false,
				Relations: map[string][]string{
					"writer":  {"user1", "user2"},
					"auditor": {"user3"},
					"viewer":  {"user4", "user5", "user6"},
					"admin":   {"user7"},
				},
				References: map[string][]string{
					"parent":  {"parent-456"},
					"project": {"project-789"},
					"team":    {"team-101"},
				},
			},
			replySubject: "reply.subject",
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				msg.On("Respond", []byte("OK")).Return(nil).Once()
				service.fgaService.client.(*MockFgaClient).On("Write", mock.Anything, mock.MatchedBy(func(req client.ClientWriteRequest) bool {
					// Should have: 3 references + 7 relations (no public) = 10 tuples
					return len(req.Writes) == 10 && len(req.Deletes) == 0
				})).Return(&client.ClientWriteResponse{}, nil)
				service.fgaService.client.(*MockFgaClient).On("Read", mock.Anything, mock.Anything, mock.Anything).Return(&client.ClientReadResponse{}, nil)
			},
			expectedError:  false,
			expectedCalled: true,
		},
		{
			name: "object with parent reference special handling",
			obj: &standardAccessStub{
				UID:        "parent-test-789",
				ObjectType: "committee",
				Public:     true,
				Relations:  map[string][]string{"owner": {"user1"}},
				References: map[string][]string{"parent": {"parent-committee-456"}},
			},
			replySubject: "reply.subject",
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				msg.On("Respond", []byte("OK")).Return(nil).Once()
				service.fgaService.client.(*MockFgaClient).On("Write", mock.Anything, mock.MatchedBy(func(req client.ClientWriteRequest) bool {
					// Should have: 1 public viewer + 1 parent (with committee: prefix) + 1 owner = 3 tuples
					if len(req.Writes) != 3 || len(req.Deletes) != 0 {
						return false
					}
					// Verify parent reference uses objectType prefix
					for _, tuple := range req.Writes {
						if tuple.Relation == "parent" {
							return tuple.User == "committee:parent-committee-456"
						}
					}
					return true
				})).Return(&client.ClientWriteResponse{}, nil)
				service.fgaService.client.(*MockFgaClient).On("Read", mock.Anything, mock.Anything, mock.Anything).Return(&client.ClientReadResponse{}, nil)
			},
			expectedError:  false,
			expectedCalled: true,
		},

		// Hard Tests - Error scenarios and edge cases
		{
			name: "missing UID should fail",
			obj: &standardAccessStub{
				UID:        "",
				ObjectType: "committee",
				Public:     true,
				Relations:  map[string][]string{"writer": {"user1"}},
				References: map[string][]string{},
			},
			replySubject: "reply.subject",
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				// No mocks needed as function should fail early
			},
			expectedError:  true,
			expectedCalled: false,
		},
		{
			name: "FGA sync failure should propagate error",
			obj: &standardAccessStub{
				UID:        "error-test-123",
				ObjectType: "groupsio_service",
				Public:     true,
				Relations:  map[string][]string{"writer": {"user1"}},
				References: map[string][]string{},
			},
			replySubject: "reply.subject",
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				// Mock FGA service to return error
				service.fgaService.client.(*MockFgaClient).On("Write", mock.Anything, mock.Anything).Return((*client.ClientWriteResponse)(nil), assert.AnError)
				service.fgaService.client.(*MockFgaClient).On("Read", mock.Anything, mock.Anything, mock.Anything).Return(&client.ClientReadResponse{}, nil)
			},
			expectedError:  true,
			expectedCalled: false,
		},
		{
			name: "empty relations and references with public access",
			obj: &standardAccessStub{
				UID:        "minimal-456",
				ObjectType: "committee",
				Public:     true,
				Relations:  map[string][]string{},
				References: map[string][]string{},
			},
			replySubject: "reply.subject",
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				msg.On("Respond", []byte("OK")).Return(nil).Once()
				service.fgaService.client.(*MockFgaClient).On("Write", mock.Anything, mock.MatchedBy(func(req client.ClientWriteRequest) bool {
					// Should have only 1 tuple: public viewer
					return len(req.Writes) == 1 && len(req.Deletes) == 0 &&
						req.Writes[0].User == "user:*" && req.Writes[0].Relation == "viewer"
				})).Return(&client.ClientWriteResponse{}, nil)
				service.fgaService.client.(*MockFgaClient).On("Read", mock.Anything, mock.Anything, mock.Anything).Return(&client.ClientReadResponse{}, nil)
			},
			expectedError:  false,
			expectedCalled: true,
		},
		{
			name: "no reply subject - should not respond",
			obj: &standardAccessStub{
				UID:        "no-reply-789",
				ObjectType: "groupsio_service",
				Public:     false,
				Relations:  map[string][]string{"writer": {"user1"}},
				References: map[string][]string{},
			},
			replySubject: "",
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				// Should not call Respond
				service.fgaService.client.(*MockFgaClient).On("Write", mock.Anything, mock.Anything).Return(&client.ClientWriteResponse{}, nil)
				service.fgaService.client.(*MockFgaClient).On("Read", mock.Anything, mock.Anything, mock.Anything).Return(&client.ClientReadResponse{}, nil)
			},
			expectedError:  false,
			expectedCalled: false,
		},
		{
			name: "respond error should propagate",
			obj: &standardAccessStub{
				UID:        "respond-error-123",
				ObjectType: "committee",
				Public:     true,
				Relations:  map[string][]string{},
				References: map[string][]string{},
			},
			replySubject: "reply.subject",
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				msg.On("Respond", []byte("OK")).Return(assert.AnError).Once()
				service.fgaService.client.(*MockFgaClient).On("Write", mock.Anything, mock.Anything).Return(&client.ClientWriteResponse{}, nil)
				service.fgaService.client.(*MockFgaClient).On("Read", mock.Anything, mock.Anything, mock.Anything).Return(&client.ClientReadResponse{}, nil)
			},
			expectedError:  true,
			expectedCalled: true,
		},

		// Hard Tests - Complex scenarios
		{
			name: "large number of relations and references",
			obj: &standardAccessStub{
				UID:        "large-scale-999",
				ObjectType: "groupsio_service",
				Public:     true,
				Relations: map[string][]string{
					"writer":  {"user1", "user2", "user3", "user4", "user5"},
					"auditor": {"user6", "user7", "user8"},
					"viewer":  {"user9", "user10", "user11", "user12"},
					"admin":   {"user13"},
					"owner":   {"user14", "user15"},
				},
				References: map[string][]string{
					"parent":     {"parent-999"},
					"project":    {"project-888"},
					"team":       {"team-777"},
					"department": {"dept-666"},
					"region":     {"region-555"},
				},
			},
			replySubject: "reply.subject",
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				msg.On("Respond", []byte("OK")).Return(nil).Once()
				service.fgaService.client.(*MockFgaClient).On("Write", mock.Anything, mock.MatchedBy(func(req client.ClientWriteRequest) bool {
					// Should have: 1 public + 5 references + 15 relations = 21 tuples
					return len(req.Writes) == 21 && len(req.Deletes) == 0
				})).Return(&client.ClientWriteResponse{}, nil)
				service.fgaService.client.(*MockFgaClient).On("Read", mock.Anything, mock.Anything, mock.Anything).Return(&client.ClientReadResponse{}, nil)
			},
			expectedError:  false,
			expectedCalled: true,
		},
		{
			name: "special characters in IDs and user names",
			obj: &standardAccessStub{
				UID:        "test-special-chars_123.456",
				ObjectType: "committee",
				Public:     false,
				Relations: map[string][]string{
					"writer": {"user:special@example.com", "user:test_user.123"},
					"viewer": {"user:another+user@domain.org"},
				},
				References: map[string][]string{
					"parent": {"parent-with_special.chars-789"},
				},
			},
			replySubject: "reply.subject",
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				msg.On("Respond", []byte("OK")).Return(nil).Once()
				service.fgaService.client.(*MockFgaClient).On("Write", mock.Anything, mock.MatchedBy(func(req client.ClientWriteRequest) bool {
					// Should have: 1 parent + 3 relations = 4 tuples (no public)
					return len(req.Writes) == 4 && len(req.Deletes) == 0
				})).Return(&client.ClientWriteResponse{}, nil)
				service.fgaService.client.(*MockFgaClient).On("Read", mock.Anything, mock.Anything, mock.Anything).Return(&client.ClientReadResponse{}, nil)
			},
			expectedError:  false,
			expectedCalled: true,
		},
		{
			name: "multiple parents with same reference name",
			obj: &standardAccessStub{
				UID:        "multi-parent-123",
				ObjectType: "committee",
				Public:     true,
				Relations:  map[string][]string{"writer": {"user1"}},
				References: map[string][]string{
					"parent": {"parent-1", "parent-2", "parent-3"},
				},
			},
			replySubject: "reply.subject",
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				msg.On("Respond", []byte("OK")).Return(nil).Once()
				service.fgaService.client.(*MockFgaClient).On("Write", mock.Anything, mock.MatchedBy(func(req client.ClientWriteRequest) bool {
					// Should have: 1 public + 3 parents + 1 writer = 5 tuples
					if len(req.Writes) != 5 || len(req.Deletes) != 0 {
						return false
					}
					// Count parent relations
					parentCount := 0
					for _, tuple := range req.Writes {
						if tuple.Relation == "parent" {
							parentCount++
							// Verify parent references use objectType prefix
							if tuple.User != "committee:parent-1" &&
							   tuple.User != "committee:parent-2" &&
							   tuple.User != "committee:parent-3" {
								return false
							}
						}
					}
					return parentCount == 3
				})).Return(&client.ClientWriteResponse{}, nil)
				service.fgaService.client.(*MockFgaClient).On("Read", mock.Anything, mock.Anything, mock.Anything).Return(&client.ClientReadResponse{}, nil)
			},
			expectedError:  false,
			expectedCalled: true,
		},
		{
			name: "multiple committees with same reference name",
			obj: &standardAccessStub{
				UID:        "multi-committee-456",
				ObjectType: "meeting",
				Public:     false,
				Relations:  map[string][]string{"organizer": {"user1", "user2"}},
				References: map[string][]string{
					"committee": {"committee-1", "committee-2", "committee-3", "committee-4"},
					"project":   {"project-789"},
				},
			},
			replySubject: "reply.subject",
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				msg.On("Respond", []byte("OK")).Return(nil).Once()
				service.fgaService.client.(*MockFgaClient).On("Write", mock.Anything, mock.MatchedBy(func(req client.ClientWriteRequest) bool {
					// Should have: 4 committees + 1 project + 2 organizers = 7 tuples
					if len(req.Writes) != 7 || len(req.Deletes) != 0 {
						return false
					}
					// Count committee relations
					committeeCount := 0
					for _, tuple := range req.Writes {
						if tuple.Relation == "committee" {
							committeeCount++
						}
					}
					return committeeCount == 4
				})).Return(&client.ClientWriteResponse{}, nil)
				service.fgaService.client.(*MockFgaClient).On("Read", mock.Anything, mock.Anything, mock.Anything).Return(&client.ClientReadResponse{}, nil)
			},
			expectedError:  false,
			expectedCalled: true,
		},
		{
			name: "multiple references of different types",
			obj: &standardAccessStub{
				UID:        "multi-ref-789",
				ObjectType: "groupsio_service",
				Public:     true,
				Relations: map[string][]string{
					"writer": {"user1"},
					"viewer": {"user2", "user3"},
				},
				References: map[string][]string{
					"parent":    {"parent-1", "parent-2"},
					"project":   {"project-1", "project-2", "project-3"},
					"committee": {"committee-1"},
					"team":      {"team-1", "team-2", "team-3", "team-4"},
				},
			},
			replySubject: "reply.subject",
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				msg.On("Respond", []byte("OK")).Return(nil).Once()
				service.fgaService.client.(*MockFgaClient).On("Write", mock.Anything, mock.MatchedBy(func(req client.ClientWriteRequest) bool {
					// Should have: 1 public + 2 parents + 3 projects + 1 committee + 4 teams + 3 relations = 14 tuples
					if len(req.Writes) != 14 || len(req.Deletes) != 0 {
						return false
					}
					// Verify counts
					counts := map[string]int{"parent": 0, "project": 0, "committee": 0, "team": 0}
					for _, tuple := range req.Writes {
						if _, ok := counts[tuple.Relation]; ok {
							counts[tuple.Relation]++
						}
					}
					return counts["parent"] == 2 && counts["project"] == 3 &&
					       counts["committee"] == 1 && counts["team"] == 4
				})).Return(&client.ClientWriteResponse{}, nil)
				service.fgaService.client.(*MockFgaClient).On("Read", mock.Anything, mock.Anything, mock.Anything).Return(&client.ClientReadResponse{}, nil)
			},
			expectedError:  false,
			expectedCalled: true,
		},
		{
			name: "empty array for reference should result in no tuples for that reference",
			obj: &standardAccessStub{
				UID:        "empty-ref-101",
				ObjectType: "committee",
				Public:     false,
				Relations:  map[string][]string{"writer": {"user1"}},
				References: map[string][]string{
					"parent":    {},
					"committee": {"committee-1", "committee-2"},
					"project":   {},
				},
			},
			replySubject: "reply.subject",
			setupMocks: func(service *HandlerService, msg *MockNatsMsg) {
				msg.On("Respond", []byte("OK")).Return(nil).Once()
				service.fgaService.client.(*MockFgaClient).On("Write", mock.Anything, mock.MatchedBy(func(req client.ClientWriteRequest) bool {
					// Should have: 2 committees + 1 writer = 3 tuples (no parent or project)
					if len(req.Writes) != 3 || len(req.Deletes) != 0 {
						return false
					}
					// Verify no parent or project relations
					for _, tuple := range req.Writes {
						if tuple.Relation == "parent" || tuple.Relation == "project" {
							return false
						}
					}
					return true
				})).Return(&client.ClientWriteResponse{}, nil)
				service.fgaService.client.(*MockFgaClient).On("Read", mock.Anything, mock.Anything, mock.Anything).Return(&client.ClientReadResponse{}, nil)
			},
			expectedError:  false,
			expectedCalled: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create message with JSON data
			messageData := []byte(`{"uid":"` + tt.obj.UID + `","object_type":"` + tt.obj.ObjectType + `"}`)
			msg := CreateMockNatsMsg(messageData)
			msg.reply = tt.replySubject

			handlerService := setupService()
			tt.setupMocks(handlerService, msg)

			// Test that the function doesn't panic
			assert.NotPanics(t, func() {
				err := handlerService.processStandardAccessUpdate(msg, tt.obj)
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
				// Ensure Respond was never called when not expected
				msg.AssertNotCalled(t, "Respond")
			}

			// Verify all mocks were called as expected
			handlerService.fgaService.client.(*MockFgaClient).AssertExpectations(t)
		})
	}
}
