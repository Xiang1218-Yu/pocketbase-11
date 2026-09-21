package core

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/dop251/goja"
)

// Computed evaluation errors.
var (
	// ErrComputedTimeout is returned when a computed field evaluation exceeds its timeout.
	ErrComputedTimeout = errors.New("computed field evaluation timeout")
	// ErrComputedScript is returned for JS runtime errors thrown during evaluation.
	ErrComputedScript = errors.New("computed field script error")
	// ErrComputedResult is returned when the evaluated result is invalid.
	ErrComputedResult = errors.New("computed field result error")
	// ErrComputedCycle is returned when a relation/expand traversal cycle is detected.
	ErrComputedCycle = errors.New("computed field relation cycle detected")
	// ErrComputedForbidden is returned when the script attempts an operation that is not allowed.
	ErrComputedForbidden = errors.New("computed field forbidden operation")
)

const computedEngineStoreKey = "@pbComputedEngine"

const (
	// computedMaxRelationDepth is the max depth of nested doc.$rel traversals.
	computedMaxRelationDepth = 6
	// computedMaxResultJSONLength caps the serialized result size of a single computed value.
	computedMaxResultJSONLength = 100_000
)

// ComputedRelationFetcher resolves the related records for the given relation field.
//
// Implementations are expected to enforce the appropriate access rules
// (see [ComputedAPIRelationFetcher] for the API/realtime behavior).
//
// The returned records are cached per [ComputeRequest].
type ComputedRelationFetcher func(record *Record, field *RelationField) ([]*Record, error)

// ComputedRequestConfig configures a single batch evaluation request.
type ComputedRequestConfig struct {
	// RequestInfo associated with the current request (used for visibility checks and $ctx).
	RequestInfo *RequestInfo

	// RelationFetcher is used to lazy-load related records from function-mode scripts.
	//
	// If nil, a default unrestricted fetcher is used (suitable for trusted server-side use).
	RelationFetcher ComputedRelationFetcher

	// ContextData is exposed to scripts as ctx.data (optional arbitrary read-only data).
	ContextData map[string]any
}

// ComputeRequest represents a single batch evaluation session.
//
// It holds per-request caches (loaded relations, evaluated computed values)
// so that multiple records and computed fields evaluated as part of the same
// request (eg. a list page) share them.
type ComputeRequest struct {
	app    App
	config ComputedRequestConfig

	relationMu sync.Mutex
	// relCache key: collectionId:recordId:fieldName -> resolved records
	relCache map[string][]*Record
	// inFlight tracks recordId:fieldName chains currently being resolved to break cycles.
	inFlight map[string]bool
}

// NewComputeRequest creates a new evaluation session.
func NewComputeRequest(app App, config ComputedRequestConfig) *ComputeRequest {
	if config.ContextData == nil {
		config.ContextData = map[string]any{}
	}
	if config.RelationFetcher == nil {
		config.RelationFetcher = defaultComputedRelationFetcher(app)
	}
	return &ComputeRequest{
		app:      app,
		config:   config,
		relCache: map[string][]*Record{},
		inFlight: map[string]bool{},
	}
}

// ComputedError wraps a single computed field evaluation failure.
type ComputedError struct {
	Collection string
	RecordId   string
	Field      string
	Err        error
}

func (e *ComputedError) Error() string {
	return fmt.Sprintf("computed field %q (collection %q, record %q): %v", e.Field, e.Collection, e.RecordId, e.Err)
}

func (e *ComputedError) Unwrap() error { return e.Err }

// ComputedEngine defines the public interface of the app-wide JS evaluation engine.
type ComputedEngine interface {
	// Evaluate runs the field's program against a single record within the request session.
	Evaluate(req *ComputeRequest, record *Record, field *ComputedField) (any, error)

	// Compile compiles (and caches) the JS program for the given field.
	Compile(field *ComputedField) error

	// Invalidate drops cached programs associated with the collection that are no
	// longer referenced by its fields (called on collection create/update/delete).
	Invalidate(collectionId string, fields []*ComputedField)
}

// computedEngine compiles and evaluates computed field scripts using a pool
// of sandboxed goja runtimes.
type computedEngine struct {
	app App

	pool *computedVMPool

	mu       sync.RWMutex
	programs map[string]*goja.Program // content-addressed by mode + "\x00" + expression
}

func newComputedEngine(app App) *computedEngine {
	engine := &computedEngine{
		app:      app,
		programs: map[string]*goja.Program{},
	}
	engine.pool = newComputedVMPool(computedVMPoolSize, engine.newSandboxVM)
	return engine
}

// ComputedEngine returns the app-wide singleton computed evaluation engine.
func (app *BaseApp) ComputedEngine() ComputedEngine {
	existing, ok := app.store.GetOk(computedEngineStoreKey)
	if e, ok2 := existing.(*computedEngine); ok && ok2 {
		return e
	}

	engine := newComputedEngine(app)

	// GetOrSet ensures only one engine is registered if multiple goroutines race
	actual := app.store.GetOrSet(computedEngineStoreKey, func() any {
		return engine
	})

	return actual.(ComputedEngine)
}

// Compile implements [ComputedEngine].
func (e *computedEngine) Compile(field *ComputedField) error {
	_, err := e.compile(field)
	return err
}

func (e *computedEngine) compile(field *ComputedField) (*goja.Program, error) {
	field.NormalizeOptions()

	key := field.Mode + "\x00" + field.Expression

	e.mu.RLock()
	program, ok := e.programs[key]
	e.mu.RUnlock()
	if ok {
		return program, nil
	}

	var src string
	switch field.Mode {
	case ComputedModeFunction:
		src = "(function(doc, ctx){\n" + field.Expression + "\n})"
	default:
		src = "(function(doc, ctx){return (" + field.Expression + ");})"
	}

	program, err := goja.Compile("computed:"+field.Name, src, true)
	if err != nil {
		return nil, err
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	// double-check after acquiring the write lock
	if existing, ok := e.programs[key]; ok {
		return existing, nil
	}
	e.programs[key] = program

	return program, nil
}

// Invalidate implements [ComputedEngine].
//
// Programs are content-addressed so changed expressions simply compile as new
// entries. Here we garbage-collect programs that are no longer referenced by
// any field of the changed collection (nor by any other tracked collection).
func (e *computedEngine) Invalidate(collectionId string, fields []*ComputedField) {
	// nothing to track per-collection since programs are content-addressed;
	// the method is kept as an explicit compatibility hook for collection
	// create/update/delete and future cache strategies.
	_ = collectionId
	_ = fields
}

// Evaluate implements [ComputedEngine].
func (e *computedEngine) Evaluate(req *ComputeRequest, record *Record, field *ComputedField) (any, error) {
	field.NormalizeOptions()

	program, err := e.compile(field)
	if err != nil {
		return nil, e.wrapErr(record, field, err)
	}

	timeout := time.Duration(field.GetTimeout()) * time.Millisecond

	var evalErr error
	var out any

	runErr := e.pool.run(func(vm *goja.Runtime) error {
		sb, ok := vm.Get(sandboxInstanceKey).Export().(*computedSandbox)
		if !ok {
			return errors.New("computed sandbox not initialized")
		}
		if err := sb.bind(req, record, field, program); err != nil {
			return err
		}

		// hard timeout: interrupt the running JS
		timer := time.AfterFunc(timeout, func() {
			vm.Interrupt(fmt.Errorf("%w after %s", ErrComputedTimeout, timeout))
		})
		defer timer.Stop()

		fn, ok := goja.AssertFunction(vm.Get("__pb_computed_run"))
		if !ok {
			return errors.New("failed to resolve the computed program entrypoint")
		}

		v, callErr := fn(goja.Undefined(), sb.docValue, sb.ctxValue)
		if callErr != nil {
			evalErr = callErr
			return nil
		}

		out = exportGojaValue(v)
		return nil
	})
	if runErr != nil {
		return nil, e.wrapErr(record, field, runErr)
	}
	if evalErr != nil {
		wrapped := evalErr

		// goja surfaces vm.Interrupt as an *goja.InterruptedError
		var interrupted *goja.InterruptedError
		if errors.As(evalErr, &interrupted) || strings.Contains(evalErr.Error(), ErrComputedTimeout.Error()) {
			wrapped = fmt.Errorf("%w: %v", ErrComputedTimeout, evalErr)
		} else {
			wrapped = fmt.Errorf("%w: %v", ErrComputedScript, evalErr)
		}
		return nil, e.wrapErr(record, field, wrapped)
	}

	if err := validateComputedResult(out, field); err != nil {
		return nil, e.wrapErr(record, field, err)
	}

	return out, nil
}

func (e *computedEngine) wrapErr(record *Record, field *ComputedField, err error) error {
	return &ComputedError{
		Collection: record.Collection().Name,
		RecordId:   record.Id,
		Field:      field.Name,
		Err:        err,
	}
}

// exportGojaValue converts a goja value to a plain Go value safe for JSON export.
func exportGojaValue(v goja.Value) any {
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return nil
	}
	return v.Export()
}

// invalidateCollectionComputedCache is invoked on collection create/update/delete.
func invalidateCollectionComputedCache(app App, collection *Collection) {
	if app == nil || collection == nil {
		return
	}
	if engine := app.ComputedEngine(); engine != nil {
		engine.Invalidate(collection.Id, ComputedFieldsOf(collection))
	}
}
