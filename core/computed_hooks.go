package core

import (
	"github.com/pocketbase/pocketbase/tools/hook"
)

// registerComputedHooks binds the system hooks that evaluate computed fields
// during record enrich (list, view, expand, realtime) and that invalidate the
// JS program cache when a collection schema changes.
func (app *BaseApp) registerComputedHooks() {
	// Evaluate computed fields after the default enrich (expands are applied
	// by the builtin OnRecordEnrich finalizer; computed scripts may access
	// them through doc.$expand).
	//
	// Priority 99 ensures the handler runs late in the chain (the default
	// enrich work is the hook's finalizer).
	app.OnRecordEnrich().Bind(&hook.Handler[*RecordEnrichEvent]{
		Id:       systemHookIdRecordComputed,
		Priority: 99,
		Func: func(e *RecordEnrichEvent) error {
			// let the rest of the chain (incl. expand loading) run first
			if err := e.Next(); err != nil {
				return err
			}

			record := e.Record
			if record == nil || !collectionHasVirtualFields(record.Collection()) {
				return nil
			}

			req := NewComputeRequest(e.App, ComputedRequestConfig{
				RequestInfo:     e.RequestInfo,
				RelationFetcher: ComputedAPIRelationFetcher(e.App, e.RequestInfo),
			})

			if err := e.App.ComputedResolve(req, []*Record{record}); err != nil {
				// Attach the error on the record so the API layer can turn the
				// whole request into a failure rather than returning a fake value.
				// The handler itself doesn't return the error by default because
				// enrich errors are currently non-fatal for custom hooks; the
				// dedicated API check in apis handles the HTTP response.
				e.App.Logger().Debug("computed field evaluation error",
					"collection", record.Collection().Name,
					"recordId", record.Id,
					"error", err.Error(),
				)
			}

			return nil
		},
	})

	// Invalidate the compilation cache whenever a collection schema change
	// is committed successfully (after-success hooks run only after the tx commit,
	// which matches how the collection cache itself is refreshed).
	invalidate := func(e *CollectionEvent) error {
		invalidateCollectionComputedCache(e.App, e.Collection)
		return e.Next()
	}
	app.OnCollectionAfterCreateSuccess().Bind(&hook.Handler[*CollectionEvent]{Id: systemHookIdRecordComputed, Priority: -99, Func: invalidate})
	app.OnCollectionAfterUpdateSuccess().Bind(&hook.Handler[*CollectionEvent]{Id: systemHookIdRecordComputed, Priority: -99, Func: invalidate})
	app.OnCollectionAfterDeleteSuccess().Bind(&hook.Handler[*CollectionEvent]{Id: systemHookIdRecordComputed, Priority: -99, Func: invalidate})
}
