// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package main provides the fga-sync service entry point and supporting types.
package main

import (
	"context"
	"fmt"
	"sort"

	openfga "github.com/openfga/go-sdk"
)

// modelSchemaVersion is the only authorization model schema version the graph
// understands.
const modelSchemaVersion = "1.1"

// relNode identifies one relation definition in the authorization model, such
// as meeting#viewer.
type relNode struct {
	typ string
	rel string
}

// modelGraph answers "if a tuple on type#relation changes, which object types
// can have their check results changed?" without calling OpenFGA. It is built
// once from the authorization model and is read-only afterwards, so it is safe
// for concurrent use.
//
// An edge A -> B means a change to relation A can change the result of
// relation B. Edges come from every way the model lets one relation feed
// another:
//
//   - computed usersets:   define viewer: writer              (T#writer -> T#viewer)
//   - tuple-to-userset:    define viewer: viewer from parent  (T#parent -> U#viewer and
//     for each type P that parent allows, P#viewer -> U#viewer)
//   - direct usersets:     define auditor: [team#member]      (team#member -> T#auditor)
//   - intersection and exclusion: every operand is a dependency, so each one
//     gets an edge. Results can flip either way, which is fine here because
//     the graph only says "may change", never "will be granted".
type modelGraph struct {
	// defined is the set of relations the model defines.
	defined map[relNode]struct{}
	// dependents maps a relation to the relations whose results may change
	// when it changes (one hop).
	dependents map[relNode][]relNode
	// affected maps a relation to the sorted object types reachable from it
	// through dependents, including its own type.
	affected map[relNode][]string
}

// newModelGraph builds the dependency graph for an authorization model. It
// returns an error for anything it cannot interpret (schema versions other
// than 1.1, relation references to undefined relations, usersets with no
// recognized rewrite). Callers must treat an error as "scoped invalidation is
// unavailable" and fall back to global invalidation, because a graph with
// missing edges would leave stale cache entries alive.
func newModelGraph(model *openfga.AuthorizationModel) (*modelGraph, error) {
	if model == nil {
		return nil, fmt.Errorf("authorization model is nil")
	}
	if model.SchemaVersion != modelSchemaVersion {
		return nil, fmt.Errorf("unsupported authorization model schema version %q", model.SchemaVersion)
	}

	g := &modelGraph{
		defined:    make(map[relNode]struct{}),
		dependents: make(map[relNode][]relNode),
		affected:   make(map[relNode][]string),
	}

	direct := make(map[relNode][]openfga.RelationReference)
	for _, td := range model.TypeDefinitions {
		if td.Relations == nil {
			continue
		}
		for rel := range *td.Relations {
			node := relNode{typ: td.Type, rel: rel}
			g.defined[node] = struct{}{}
			if td.Metadata != nil && td.Metadata.Relations != nil {
				if meta, ok := (*td.Metadata.Relations)[rel]; ok && meta.DirectlyRelatedUserTypes != nil {
					direct[node] = *meta.DirectlyRelatedUserTypes
				}
			}
		}
	}

	for _, td := range model.TypeDefinitions {
		if td.Relations == nil {
			continue
		}
		for rel, userset := range *td.Relations {
			target := relNode{typ: td.Type, rel: rel}
			if err := g.addEdges(target, userset, direct); err != nil {
				return nil, fmt.Errorf("%s#%s: %w", td.Type, rel, err)
			}
		}
	}

	for node := range g.defined {
		g.affected[node] = g.reachableTypes(node)
	}
	return g, nil
}

// addEdges records the edges feeding target from one userset expression.
func (g *modelGraph) addEdges(
	target relNode,
	u openfga.Userset,
	direct map[relNode][]openfga.RelationReference,
) error {
	switch {
	case u.This != nil:
		for _, ref := range direct[target] {
			if ref.Relation == nil {
				// Plain type or wildcard: the tuple itself is the dependency,
				// and its relation is the start node of any closure.
				continue
			}
			src := relNode{typ: ref.Type, rel: *ref.Relation}
			if err := g.requireDefined(src); err != nil {
				return err
			}
			g.addEdge(src, target)
		}
		return nil

	case u.ComputedUserset != nil:
		if u.ComputedUserset.Relation == nil {
			return fmt.Errorf("computed userset has no relation")
		}
		src := relNode{typ: target.typ, rel: *u.ComputedUserset.Relation}
		if err := g.requireDefined(src); err != nil {
			return err
		}
		g.addEdge(src, target)
		return nil

	case u.TupleToUserset != nil:
		ttu := u.TupleToUserset
		if ttu.Tupleset.Relation == nil || ttu.ComputedUserset.Relation == nil {
			return fmt.Errorf("tuple-to-userset is missing a relation")
		}
		tupleset := relNode{typ: target.typ, rel: *ttu.Tupleset.Relation}
		if err := g.requireDefined(tupleset); err != nil {
			return err
		}
		// Changing which parents exist changes the result.
		g.addEdge(tupleset, target)
		// Changing the computed relation on any allowed parent type changes
		// it too. A parent type that does not define the relation simply
		// contributes nothing, matching OpenFGA's evaluation.
		for _, ref := range direct[tupleset] {
			parent := relNode{typ: ref.Type, rel: *ttu.ComputedUserset.Relation}
			if _, ok := g.defined[parent]; ok {
				g.addEdge(parent, target)
			}
		}
		return nil

	case u.Union != nil:
		return g.addChildEdges(target, u.Union.Child, direct)

	case u.Intersection != nil:
		return g.addChildEdges(target, u.Intersection.Child, direct)

	case u.Difference != nil:
		return g.addChildEdges(target, []openfga.Userset{u.Difference.Base, u.Difference.Subtract}, direct)
	}
	return fmt.Errorf("userset has no recognized rewrite")
}

func (g *modelGraph) addChildEdges(
	target relNode,
	children []openfga.Userset,
	direct map[relNode][]openfga.RelationReference,
) error {
	for _, child := range children {
		if err := g.addEdges(target, child, direct); err != nil {
			return err
		}
	}
	return nil
}

func (g *modelGraph) requireDefined(node relNode) error {
	if _, ok := g.defined[node]; !ok {
		return fmt.Errorf("reference to undefined relation %s#%s", node.typ, node.rel)
	}
	return nil
}

func (g *modelGraph) addEdge(from, to relNode) {
	for _, existing := range g.dependents[from] {
		if existing == to {
			return
		}
	}
	g.dependents[from] = append(g.dependents[from], to)
}

// reachableTypes walks dependents from start (cycle-safe) and returns the
// sorted set of object types touched, including start's own type.
func (g *modelGraph) reachableTypes(start relNode) []string {
	seen := map[relNode]struct{}{start: {}}
	queue := []relNode{start}
	types := map[string]struct{}{start.typ: {}}
	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]
		for _, next := range g.dependents[node] {
			if _, ok := seen[next]; ok {
				continue
			}
			seen[next] = struct{}{}
			types[next.typ] = struct{}{}
			queue = append(queue, next)
		}
	}
	out := make([]string, 0, len(types))
	for typ := range types {
		out = append(out, typ)
	}
	sort.Strings(out)
	return out
}

// affectedTypes returns the object types whose cached check results can change
// when a tuple on typ#rel is written or deleted. ok is false when the model
// does not define that relation, in which case callers must invalidate
// globally.
func (g *modelGraph) affectedTypes(typ, rel string) ([]string, bool) {
	types, ok := g.affected[relNode{typ: typ, rel: rel}]
	return types, ok
}

// loadModelGraph fetches the authorization model this service is configured
// to use and builds its dependency graph.
func loadModelGraph(ctx context.Context, client IFgaClient) (*modelGraph, error) {
	model, err := client.ReadAuthorizationModel(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading authorization model: %w", err)
	}
	return newModelGraph(model)
}
