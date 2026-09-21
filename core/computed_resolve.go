package core

import (
	"fmt"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/tools/search"
)

// ComputedVisibilityError marks a computed field that had to be hidden because
// one of its dependency fields is hidden/inaccessible for the request.
//
// It is exposed for callers that want to distinguish visibility-driven hiding
// from other evaluation outcomes.
type ComputedVisibilityError struct{ Field string }

func (e *ComputedVisibilityError) Error() string {
	return fmt.Sprintf("computed field %q is hidden because it depends on an inaccessible field", e.Field)
}

var _ error = (*ComputedVisibilityError)(nil)

// ComputedResolve evaluates all (or a subset of) computed fields of the given
// records within the provided request session and stores the evaluated values
// back on each record.
//
// Computed fields depending on hidden/inaccessible dependency fields are
// hidden for the request instead of being evaluated.
//
// The method processes computed fields in dependency order (topologically),
// returning an error on cyclic dependencies (which should normally be caught
// at settings validation time).
func (app *BaseApp) ComputedResolve(req *ComputeRequest, records []*Record, optFieldNames ...string) error {
	if len(records) == 0 {
		return nil
	}

	collection := records[0].Collection()
	if collection == nil {
		return fmt.Errorf("computed resolve: missing collection")
	}

	fields := orderedComputedFields(collection)

	wanted := map[string]bool{}
	if len(optFieldNames) > 0 {
		for _, n := range optFieldNames {
			wanted[n] = true
		}
	}

	engine := app.ComputedEngine()

	for _, field := range fields {
		if len(wanted) > 0 && !wanted[field.Name] {
			continue
		}

		for _, record := range records {
			// skip already evaluated in this session (cached on the record,
			// including the nil marker used for hidden computed fields)
			if _, evaluated, _ := record.computedStoreGet(field.Name); evaluated {
				continue
			}

			// visibility: hidden field itself or hidden/inaccessible dependency
			if !computedIsVisible(req, record, field) {
				record.Hide(field.Name)
				record.computedStoreSet(field.Name, (*computedValueMarker)(nil))
				continue
			}

			value, err := engine.Evaluate(req, record, field)
			if err != nil {
				record.computedStoreSetError(field.Name, err)
				return err
			}

			record.computedStoreSet(field.Name, &computedValueMarker{value: value})
		}
	}

	return nil
}

// ComputedResolveOne is a convenience wrapper around [BaseApp.ComputedResolve]
// for a single record without an explicit request context (server-side usage).
func (app *BaseApp) ComputedResolveOne(record *Record) error {
	req := NewComputeRequest(app, ComputedRequestConfig{})
	return app.ComputedResolve(req, []*Record{record})
}

// ComputedErrors returns all computed evaluation errors attached to the record
// during a previous [BaseApp.ComputedResolve] call.
func (record *Record) ComputedErrors() map[string]error {
	return record.computedErrorsSnapshot()
}

// orderedComputedFields returns all computed fields of the collection in
// topological order based on their DependsOn graph.
func orderedComputedFields(collection *Collection) []*ComputedField {
	computed := ComputedFieldsOf(collection)

	nameToField := map[string]*ComputedField{}
	for _, f := range computed {
		nameToField[f.Name] = f
	}

	state := map[string]int{} // 0=unvisited 1=in progress 2=done
	sorted := make([]*ComputedField, 0, len(computed))

	var visit func(f *ComputedField)
	visit = func(f *ComputedField) {
		switch state[f.Name] {
		case 1:
			return // cycle is validated at settings level; skip to avoid loops
		case 2:
			return
		}
		state[f.Name] = 1
		for _, dep := range f.DependsOn {
			if depField, ok := nameToField[dep]; ok {
				visit(depField)
			}
		}
		state[f.Name] = 2
		sorted = append(sorted, f)
	}

	// declaration order for deterministic output when there are no deps
	for _, f := range computed {
		visit(f)
	}

	return sorted
}

// computedFieldVisibleForRequest reports whether the given (computed or
// regular) field of the record is visible for the current request.
func computedFieldVisibleForRequest(info *RequestInfo, record *Record, field Field) bool {
	if info == nil || info.HasSuperuserAuth() {
		return true
	}
	if visible, ok := record.customVisibility.GetOk(field.GetName()); ok {
		return visible
	}
	return !field.GetHidden()
}

// computedIsVisible reports whether the computed field is visible for the
// current request. A computed field is hidden when:
//   - the field itself is hidden (standard rule, also enforced on export), or
//   - any of its dependency fields is hidden/inaccessible for the request
//     (so that scripts cannot leak hidden data through their output).
func computedIsVisible(req *ComputeRequest, record *Record, field *ComputedField) bool {
	info := req.config.RequestInfo

	if !computedFieldVisibleForRequest(info, record, field) {
		return false
	}

	// superusers/server-side bypass dependency checks
	if info == nil || info.HasSuperuserAuth() {
		return true
	}

	// dependency visibility
	for _, dep := range field.DependsOn {
		depField := record.Collection().Fields.GetByName(dep)
		if depField == nil {
			continue
		}
		if !computedFieldVisibleForRequest(info, record, depField) {
			return false
		}
	}

	return true
}

// defaultComputedRelationFetcher is the unrestricted server-side relation
// fetcher used when no request context is present.
func defaultComputedRelationFetcher(app App) ComputedRelationFetcher {
	return func(record *Record, field *RelationField) ([]*Record, error) {
		ids := record.GetStringSlice(field.Name)
		if len(ids) == 0 {
			return nil, nil
		}
		return app.FindRecordsByIds(field.CollectionId, ids)
	}
}

// ComputedAPIRelationFetcher builds a relation fetcher that enforces the
// related collection's ViewRule for the given request info.
//
// It mirrors the expand fetch behavior used by the regular record APIs so
// computed values that traverse relations cannot bypass API rules.
func ComputedAPIRelationFetcher(app App, info *RequestInfo) ComputedRelationFetcher {
	if info == nil {
		return defaultComputedRelationFetcher(app)
	}

	return func(record *Record, field *RelationField) ([]*Record, error) {
		ids := record.GetStringSlice(field.Name)
		if len(ids) == 0 {
			return nil, nil
		}

		relCollection, err := app.FindCachedCollectionByNameOrId(field.CollectionId)
		if err != nil || relCollection == nil {
			return nil, nil
		}

		if info.HasSuperuserAuth() {
			return app.FindRecordsByIds(relCollection.Id, ids)
		}

		if relCollection.ViewRule == nil {
			// only superusers can access; return no relations (script sees null/[])
			return nil, nil
		}

		ruleFunc := func(q *dbx.SelectQuery) error {
			if *relCollection.ViewRule == "" {
				return nil // public
			}
			resolver := NewRecordFieldResolver(app, relCollection, info, true)
			expr, err := search.FilterData(*relCollection.ViewRule).BuildExpr(resolver)
			if err != nil {
				return err
			}
			q.AndWhere(expr)
			return resolver.UpdateQuery(q)
		}

		rels, err := app.FindRecordsByIds(relCollection.Id, ids, ruleFunc)
		if err != nil {
			return nil, err
		}

		// apply standard visibility flags to the fetched related records too
		// (eg. hidden fields / email visibility), consistent with expand
		for _, rel := range rels {
			if !info.HasSuperuserAuth() {
				for _, f := range relCollection.Fields {
					if f.GetHidden() {
						rel.Hide(f.GetName())
					}
				}
			}
		}

		return rels, nil
	}
}
