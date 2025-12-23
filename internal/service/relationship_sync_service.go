// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"

	openfga "github.com/openfga/go-sdk"
	"github.com/openfga/go-sdk/client"
)

// RelationshipSynchronizer defines the behavior needed to synchronize relationships.
// This keeps the policy handler decoupled from the main FgaService implementation.
// Notes:
// * The actual FgaService from the main package implements this interface.
// * We might consider refactoring this, since is too tied to the client package (anti-pattern).
type RelationshipSynchronizer interface {
	TupleKey(user, relation, object string) client.ClientTupleKey
	TupleKeyWithoutCondition(user, relation, object string) client.ClientTupleKeyWithoutCondition
	ReadObjectTuples(ctx context.Context, object string) ([]openfga.Tuple, error)
	WriteAndDeleteTuples(ctx context.Context, writes []client.ClientTupleKey, deletes []client.ClientTupleKeyWithoutCondition) error
}
