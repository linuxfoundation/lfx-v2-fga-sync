// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/linuxfoundation/lfx-v2-fga-sync/internal/domain"
	openfga "github.com/openfga/go-sdk"
	"github.com/openfga/go-sdk/client"
)

// newDiscardLogger creates a logger that discards all output
func newDiscardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// mockRelationshipSynchronizer is a mock implementation of RelationshipSynchronizer for testing
type mockRelationshipSynchronizer struct {
	tuples                []openfga.Tuple
	writeCalled           bool
	writeError            error
	readError             error
	writtenTuples         []client.ClientTupleKey
	deletedTuples         []client.ClientTupleKeyWithoutCondition
	readObjectTuplesCalls int
}

func (m *mockRelationshipSynchronizer) TupleKey(user, relation, object string) client.ClientTupleKey {
	return client.ClientTupleKey{
		User:     user,
		Relation: relation,
		Object:   object,
	}
}

func (m *mockRelationshipSynchronizer) TupleKeyWithoutCondition(user, relation, object string) client.ClientTupleKeyWithoutCondition {
	return client.ClientTupleKeyWithoutCondition{
		User:     user,
		Relation: relation,
		Object:   object,
	}
}

func (m *mockRelationshipSynchronizer) ReadObjectTuples(ctx context.Context, object string) ([]openfga.Tuple, error) {
	m.readObjectTuplesCalls++
	if m.readError != nil {
		return nil, m.readError
	}
	return m.tuples, nil
}

func (m *mockRelationshipSynchronizer) WriteAndDeleteTuples(ctx context.Context, writes []client.ClientTupleKey, deletes []client.ClientTupleKeyWithoutCondition) error {
	m.writeCalled = true
	m.writtenTuples = writes
	m.deletedTuples = deletes
	if m.writeError != nil {
		return m.writeError
	}
	return nil
}

func TestPolicyHandler_EvaluatePolicy_ValidationErrors(t *testing.T) {
	ctx := context.Background()
	mock := &mockRelationshipSynchronizer{}
	handler := NewPolicyHandler(newDiscardLogger(), mock)

	tests := []struct {
		name     string
		policy   domain.Policy
		objectID string
		wantErr  bool
		errMsg   string
	}{
		{
			name: "empty policy name",
			policy: domain.Policy{
				Name:     "",
				Value:    "basic_profile",
				Relation: "allows_basic_profile",
			},
			objectID: "committee:123",
			wantErr:  true,
			errMsg:   "policy name cannot be empty",
		},
		{
			name: "empty policy value",
			policy: domain.Policy{
				Name:     "visibility_policy",
				Value:    "",
				Relation: "allows_basic_profile",
			},
			objectID: "committee:123",
			wantErr:  true,
			errMsg:   "policy value cannot be empty",
		},
		{
			name: "empty policy relation",
			policy: domain.Policy{
				Name:     "visibility_policy",
				Value:    "basic_profile",
				Relation: "",
			},
			objectID: "committee:123",
			wantErr:  true,
			errMsg:   "policy relation cannot be empty",
		},
		{
			name: "empty object ID",
			policy: domain.Policy{
				Name:     "visibility_policy",
				Value:    "basic_profile",
				Relation: "allows_basic_profile",
			},
			objectID: "",
			wantErr:  true,
			errMsg:   "object ID is required for policy evaluation",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := handler.EvaluatePolicy(ctx, tt.policy, tt.objectID, "member")
			if (err != nil) != tt.wantErr {
				t.Errorf("EvaluatePolicy() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if err != nil && err.Error() != tt.errMsg {
				t.Errorf("EvaluatePolicy() error message = %v, want %v", err.Error(), tt.errMsg)
			}
		})
	}
}

func TestPolicyHandler_EvaluatePolicy_NoExistingTuples(t *testing.T) {
	ctx := context.Background()
	mock := &mockRelationshipSynchronizer{
		tuples: []openfga.Tuple{}, // No existing tuples
	}
	handler := NewPolicyHandler(newDiscardLogger(), mock)

	policy := domain.Policy{
		Name:     "visibility_policy",
		Value:    "basic_profile",
		Relation: "allows_basic_profile",
	}
	objectID := "committee:f01dec3e-2611-482e-bffc-b4a6d9cd0afd"

	err := handler.EvaluatePolicy(ctx, policy, objectID, "member")
	if err != nil {
		t.Errorf("EvaluatePolicy() unexpected error = %v", err)
	}

	// Verify write was called
	if !mock.writeCalled {
		t.Error("Expected WriteAndDeleteTuples to be called")
	}

	// Verify two tuples were written
	if len(mock.writtenTuples) != 2 {
		t.Errorf("Expected 2 tuples to be written, got %d", len(mock.writtenTuples))
	}

	// Verify no tuples were deleted
	if len(mock.deletedTuples) != 0 {
		t.Errorf("Expected 0 tuples to be deleted, got %d", len(mock.deletedTuples))
	}

	// Verify the first tuple (object to policy)
	expectedTuple1 := client.ClientTupleKey{
		User:     "visibility_policy:basic_profile",
		Relation: "visibility_policy",
		Object:   "committee:f01dec3e-2611-482e-bffc-b4a6d9cd0afd",
	}
	if mock.writtenTuples[0] != expectedTuple1 {
		t.Errorf("First tuple = %+v, want %+v", mock.writtenTuples[0], expectedTuple1)
	}

	// Verify the second tuple (policy to user relation)
	expectedTuple2 := client.ClientTupleKey{
		User:     "committee:f01dec3e-2611-482e-bffc-b4a6d9cd0afd#member",
		Relation: "allows_basic_profile",
		Object:   "visibility_policy:basic_profile",
	}
	if mock.writtenTuples[1] != expectedTuple2 {
		t.Errorf("Second tuple = %+v, want %+v", mock.writtenTuples[1], expectedTuple2)
	}

	// Verify ReadObjectTuples was called twice (once for each object)
	if mock.readObjectTuplesCalls != 2 {
		t.Errorf("Expected ReadObjectTuples to be called 2 times, got %d", mock.readObjectTuplesCalls)
	}
}

func TestPolicyHandler_EvaluatePolicy_ExistingTuples(t *testing.T) {
	ctx := context.Background()

	// Setup mock with existing tuples
	existingTuple1 := openfga.Tuple{
		Key: openfga.TupleKey{
			User:     "visibility_policy:basic_profile",
			Relation: "visibility_policy",
			Object:   "committee:f01dec3e-2611-482e-bffc-b4a6d9cd0afd",
		},
	}
	existingTuple2 := openfga.Tuple{
		Key: openfga.TupleKey{
			User:     "committee:f01dec3e-2611-482e-bffc-b4a6d9cd0afd#member",
			Relation: "allows_basic_profile",
			Object:   "visibility_policy:basic_profile",
		},
	}

	mock := &mockRelationshipSynchronizer{
		tuples: []openfga.Tuple{existingTuple1, existingTuple2},
	}
	handler := NewPolicyHandler(newDiscardLogger(), mock)

	policy := domain.Policy{
		Name:     "visibility_policy",
		Value:    "basic_profile",
		Relation: "allows_basic_profile",
	}
	objectID := "committee:f01dec3e-2611-482e-bffc-b4a6d9cd0afd"

	err := handler.EvaluatePolicy(ctx, policy, objectID, "member")
	if err != nil {
		t.Errorf("EvaluatePolicy() unexpected error = %v", err)
	}

	// Verify write was NOT called since tuples already exist
	if mock.writeCalled {
		t.Error("Expected WriteAndDeleteTuples NOT to be called when tuples already exist")
	}
}

func TestPolicyHandler_EvaluatePolicy_ConflictingTuples(t *testing.T) {
	ctx := context.Background()

	// Setup mock with conflicting tuple (wrong relation)
	conflictingTuple := openfga.Tuple{
		Key: openfga.TupleKey{
			User:     "visibility_policy:basic_profile",
			Relation: "old_relation", // Wrong relation
			Object:   "committee:f01dec3e-2611-482e-bffc-b4a6d9cd0afd",
		},
	}

	mock := &mockRelationshipSynchronizer{
		tuples: []openfga.Tuple{conflictingTuple},
	}
	handler := NewPolicyHandler(newDiscardLogger(), mock)

	policy := domain.Policy{
		Name:     "visibility_policy",
		Value:    "basic_profile",
		Relation: "allows_basic_profile",
	}
	objectID := "committee:f01dec3e-2611-482e-bffc-b4a6d9cd0afd"

	err := handler.EvaluatePolicy(ctx, policy, objectID, "member")
	if err != nil {
		t.Errorf("EvaluatePolicy() unexpected error = %v", err)
	}

	// Verify write was called
	if !mock.writeCalled {
		t.Error("Expected WriteAndDeleteTuples to be called")
	}

	// Verify one tuple was deleted (the conflicting one)
	if len(mock.deletedTuples) != 1 {
		t.Errorf("Expected 1 tuple to be deleted, got %d", len(mock.deletedTuples))
	}

	// Verify the correct tuples were written (two new ones)
	if len(mock.writtenTuples) != 2 {
		t.Errorf("Expected 2 tuples to be written, got %d", len(mock.writtenTuples))
	}
}

func TestPolicyHandler_EvaluatePolicy_ReadError(t *testing.T) {
	ctx := context.Background()
	expectedError := errors.New("read error")
	mock := &mockRelationshipSynchronizer{
		readError: expectedError,
	}
	handler := NewPolicyHandler(newDiscardLogger(), mock)

	policy := domain.Policy{
		Name:     "visibility_policy",
		Value:    "basic_profile",
		Relation: "allows_basic_profile",
	}
	objectID := "committee:123"

	err := handler.EvaluatePolicy(ctx, policy, objectID, "member")
	if err == nil {
		t.Error("Expected error, got nil")
	}

	// Verify write was NOT called due to read error
	if mock.writeCalled {
		t.Error("Expected WriteAndDeleteTuples NOT to be called when read fails")
	}
}

func TestPolicyHandler_EvaluatePolicy_WriteError(t *testing.T) {
	ctx := context.Background()
	expectedError := errors.New("write error")
	mock := &mockRelationshipSynchronizer{
		tuples:     []openfga.Tuple{}, // No existing tuples
		writeError: expectedError,
	}
	handler := NewPolicyHandler(newDiscardLogger(), mock)

	policy := domain.Policy{
		Name:     "visibility_policy",
		Value:    "basic_profile",
		Relation: "allows_basic_profile",
	}
	objectID := "committee:123"

	err := handler.EvaluatePolicy(ctx, policy, objectID, "member")
	if err == nil {
		t.Error("Expected error, got nil")
	}

	if !errors.Is(err, expectedError) {
		t.Errorf("Expected write error, got: %v", err)
	}
}

func TestPolicyHandler_EvaluatePolicy_DifferentPolicies(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name     string
		policy   domain.Policy
		objectID string
	}{
		{
			name: "visibility policy",
			policy: domain.Policy{
				Name:     "visibility_policy",
				Value:    "basic_profile",
				Relation: "allows_basic_profile",
			},
			objectID: "committee:123",
		},
		{
			name: "access policy",
			policy: domain.Policy{
				Name:     "access_policy",
				Value:    "admin",
				Relation: "allows_admin",
			},
			objectID: "project:abc",
		},
		{
			name: "privacy policy",
			policy: domain.Policy{
				Name:     "privacy_policy",
				Value:    "public",
				Relation: "allows_public_view",
			},
			objectID: "team:xyz",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &mockRelationshipSynchronizer{
				tuples: []openfga.Tuple{},
			}
			handler := NewPolicyHandler(newDiscardLogger(), mock)

			err := handler.EvaluatePolicy(ctx, tt.policy, tt.objectID, "member")
			if err != nil {
				t.Errorf("EvaluatePolicy() unexpected error = %v", err)
			}

			// Verify tuples were written correctly
			if len(mock.writtenTuples) != 2 {
				t.Errorf("Expected 2 tuples, got %d", len(mock.writtenTuples))
			}

			// Verify first tuple uses policy ObjectID
			expectedPolicyObject := tt.policy.ObjectID()
			if mock.writtenTuples[0].User != expectedPolicyObject {
				t.Errorf("First tuple user = %v, want %v", mock.writtenTuples[0].User, expectedPolicyObject)
			}

			// Verify second tuple uses policy UserRelation
			expectedUserRelation := tt.policy.UserRelation(tt.objectID, "member")
			if mock.writtenTuples[1].User != expectedUserRelation {
				t.Errorf("Second tuple user = %v, want %v", mock.writtenTuples[1].User, expectedUserRelation)
			}
		})
	}
}

func TestNewPolicyHandler(t *testing.T) {
	mock := &mockRelationshipSynchronizer{}
	handler := NewPolicyHandler(newDiscardLogger(), mock)

	if handler == nil {
		t.Error("NewPolicyHandler returned nil")
	}

	// Verify the handler implements PolicyHandler interface
	var _ PolicyHandler = handler
}
