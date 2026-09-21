package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/dop251/goja"
	json "encoding/json/v2"
	"github.com/pocketbase/pocketbase/tools/list"
	"github.com/pocketbase/pocketbase/tools/types"
)

// ErrComputedField is returned when a read-only computed field cannot be
// evaluated for the current request.
var ErrComputedField = errors.New("computed field error")

type computedRecordContext struct {
	app         App
	requestInfo *RequestInfo
	prepared    map[string]bool
}

type computedRuntime struct {
	vm       *goja.Runtime
	builtins map[string]struct{}
}

var (
	computedProgramMu    sync.RWMutex
	computedProgramCache = map[string]*goja.Program{}
	computedRuntimePool  = sync.Pool{}
)

// BindComputedContext attaches the app and request information used to
// evaluate computed fields and declared relation dependencies.
func (m *Record) BindComputedContext(app App, requestInfo *RequestInfo) *Record {
	if m.computed == nil {
		m.computed = &computedRecordContext{prepared: map[string]bool{}}
	}
	m.computed.app = app
	m.computed.requestInfo = requestInfo
	return m
}

// ComputedRequestInfo returns the auth/request context associated with the
// record during computed-field enrichment, if any.
func (m *Record) ComputedRequestInfo() *RequestInfo {
	if m.computed == nil {
		return nil
	}
	return m.computed.requestInfo
}

type computedProgram struct {
	field   *ComputedField
	program *goja.Program
}

func ResetComputedProgramCache() {
	computedProgramMu.Lock()
	clear(computedProgramCache)
	computedProgramMu.Unlock()
}

func newComputedProgram(field *ComputedField) (*computedProgram, error) {
	source := ""
	if field.normalizedMode() == ComputedModeFunction {
		source = "(function(record, relation){" + field.Function + "})"
	} else {
		source = "(function(record, relation){return (" + field.Expression + ");})"
	}

	computedProgramMu.RLock()
	p, ok := computedProgramCache[source]
	computedProgramMu.RUnlock()
	if ok {
		return &computedProgram{field: field, program: p}, nil
	}

	compiled, err := goja.Compile("computed-field.js", source, false)
	if err != nil {
		return nil, err
	}

	computedProgramMu.Lock()
	computedProgramCache[source] = compiled
	computedProgramMu.Unlock()

	return &computedProgram{field: field, program: compiled}, nil
}

func newComputedRuntime() *computedRuntime {
	vm := goja.New()
	locked := map[string]struct{}{}
	for _, name := range []string{
		"eval", "Function", "Promise", "Proxy", "Reflect", "WeakMap", "WeakSet",
		"setTimeout", "setInterval", "setImmediate", "queueMicrotask",
		"fetch", "XMLHttpRequest", "process", "require",
	} {
		_ = vm.Set(name, goja.Undefined())
		locked[name] = struct{}{}
	}
	script := `
(function(){
  var builtins = ['Object','Array','String','Number','Boolean','Math','JSON','Date'];
  for (var i=0;i<builtins.length;i++) {
    var ctor = globalThis[builtins[i]];
    if (ctor) Object.freeze(ctor);
    if (ctor && ctor.prototype) Object.freeze(ctor.prototype);
  }
})();`
	if _, err := vm.RunString(script); err != nil {
		panic(err)
	}
	return &computedRuntime{vm: vm, builtins: locked}
}

func runComputedProgram(p *computedProgram, recordValue map[string]any, relationFunc func(string) (any, error)) (result any, err error) {
	timeout := time.Duration(p.field.normalizedTimeout()) * time.Millisecond
	cr := getComputedRuntime()
	vm := cr.vm
	defer putComputedRuntime(cr)

	vm.SetFieldNameMapper(goja.UncapFieldNameMapper())
	for name := range cr.builtins {
		_ = vm.Set(name, goja.Undefined())
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	stop := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			vm.Interrupt("computed field timeout")
		case <-stop:
		}
	}()
	defer close(stop)

	if err := vm.Set("relation", func(call goja.FunctionCall) goja.Value {
		path := strings.TrimSpace(call.Argument(0).String())
		if path == "" {
			panic(vm.NewTypeError("relation path is required"))
		}
		v, err := relationFunc(path)
		if err != nil {
			panic(vm.NewTypeError(err.Error()))
		}
		return vm.ToValue(v)
	}); err != nil {
		return nil, err
	}

	fn, err := vm.RunProgram(p.program)
	if err != nil {
		return nil, normalizeComputedError(err)
	}

	call, ok := goja.AssertFunction(fn)
	if !ok {
		return nil, fmt.Errorf("%w: compiled script is not a function", ErrComputedField)
	}

	recordArg := vm.ToValue(recordValue)
	relationArg := goja.Undefined()
	v, err := call(goja.Undefined(), recordArg, relationArg)
	if err != nil {
		return nil, normalizeComputedError(err)
	}

	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return nil, nil
	}

	return normalizeComputedResult(v.Export())
}

func getComputedRuntime() *computedRuntime {
	if vm := computedRuntimePool.Get(); vm != nil {
		return vm.(*computedRuntime)
	}
	return newComputedRuntime()
}

func putComputedRuntime(cr *computedRuntime) {
	cr.vm.ClearInterrupt()
	_ = cr.vm.Set("relation", goja.Undefined())
	computedRuntimePool.Put(cr)
}

func normalizeComputedError(err error) error {
	if err == nil {
		return nil
	}
	var exception *goja.Exception
	if errors.As(err, &exception) {
		return fmt.Errorf("%w: %s", ErrComputedField, exception.String())
	}
	var interruptErr *goja.InterruptedError
	if errors.As(err, &interruptErr) {
		return fmt.Errorf("%w: timeout", ErrComputedField)
	}
	return fmt.Errorf("%w: %v", ErrComputedField, err)
}

func normalizeComputedResult(v any) (any, error) {
	switch x := v.(type) {
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64, bool, string, nil:
		return x, nil
	default:
		// JSON normalization blocks functions/symbols and guarantees API-safe
		// primitive/object/array values.
		raw, err := json.Marshal(x)
		if err != nil {
			return nil, fmt.Errorf("%w: result is not JSON serializable", ErrComputedField)
		}
		var result any
		if err := json.Unmarshal(raw, &result); err != nil {
			return nil, err
		}
		return result, nil
	}
}

// EvaluateComputedField evaluates a single computed field.
// app may be nil only for fields that do not declare relation dependencies.
func EvaluateComputedField(ctx context.Context, app App, record *Record, field *ComputedField, active ...string) (any, error) {
	if app == nil && record.computed != nil {
		app = record.computed.app
	}
	var requestInfo *RequestInfo
	if record.computed != nil {
		requestInfo = record.computed.requestInfo
	}
	values, err := PrepareComputedFields(ctx, app, requestInfo, []*Record{record}, []string{field.GetName()}, active...)
	if err != nil {
		return nil, err
	}
	return values[record][field.GetName()], nil
}

// PrepareComputedFields evaluates the requested computed fields and stores
// them in each record's in-memory data. A failure for one record/field fails
// the entire batch, preventing partial API responses with placeholder values.
func PrepareComputedFields(
	ctx context.Context,
	app App,
	requestInfo *RequestInfo,
	records []*Record,
	only []string,
	active ...string,
) (map[*Record]map[string]any, error) {
	if len(records) == 0 {
		return map[*Record]map[string]any{}, nil
	}

	collection := records[0].Collection()
	allComputed := map[string]*ComputedField{}
	for _, f := range collection.Fields {
		if c, ok := f.(*ComputedField); ok {
			allComputed[f.GetName()] = c
		}
	}
	if len(allComputed) == 0 {
		return map[*Record]map[string]any{}, nil
	}

	requested := allComputed
	if len(only) > 0 {
		requested = map[string]*ComputedField{}
		for _, name := range list.ToUniqueStringSlice(only) {
			f := allComputed[name]
			if f == nil {
				return nil, fmt.Errorf("%w: unknown computed field %q", ErrComputedField, name)
			}
			requested[name] = f
		}
	}

	for _, record := range records {
		record.BindComputedContext(app, requestInfo)
	}

	// Topological order enforces acyclic local dependencies. Runtime "active"
	// also guards dynamically declared dependencies when Deps is omitted.
	ordered, err := orderedComputedFields(collection, requested)
	if err != nil {
		return nil, err
	}

	results := make(map[*Record]map[string]any, len(records))
	for _, record := range records {
		results[record] = map[string]any{}
	}

	activeSet := map[string]bool{}
	for _, name := range active {
		activeSet[name] = true
	}

	for _, field := range ordered {
		p, err := newComputedProgram(field)
		if err != nil {
			return nil, fmt.Errorf("%w: %s: %v", ErrComputedField, field.Name, err)
		}

		allowedDeps := map[string]bool{}
		for _, dep := range list.ToUniqueStringSlice(field.Deps) {
			allowedDeps[dep] = true
		}

		for _, record := range records {
			name := field.Name
			if record.computed != nil && record.computed.prepared[name] {
				results[record][name] = record.GetRaw(name)
				continue
			}
			if activeSet[name] {
				return nil, fmt.Errorf("%w: recursive dependency involving %q", ErrComputedField, name)
			}
			activeSet[name] = true

			recordValue := computedRecordValue(record, allowedDeps, requestInfo, activeSet)
			relationFn := func(path string) (any, error) {
				return computedRelationValue(app, record, path, requestInfo, field, activeSet)
			}
			value, err := runComputedProgram(p, recordValue, relationFn)
			if err != nil {
				return nil, fmt.Errorf("%w (%s.%s): %w", ErrComputedField, collection.Name, name, err)
			}
			value, err = normalizeComputedResult(value)
			if err != nil {
				return nil, fmt.Errorf("%w (%s.%s): %w", ErrComputedField, collection.Name, name, err)
			}

			record.SetRaw(name, value)
			if record.computed == nil {
				record.computed = &computedRecordContext{prepared: map[string]bool{}}
			}
			if record.computed.prepared == nil {
				record.computed.prepared = map[string]bool{}
			}
			record.computed.prepared[name] = true
			results[record][name] = value
			delete(activeSet, name)
		}
	}

	return results, nil
}

func orderedComputedFields(collection *Collection, requested map[string]*ComputedField) ([]*ComputedField, error) {
	visiting := map[string]bool{}
	visited := map[string]bool{}
	result := []*ComputedField{}

	var visit func(name string) error
	visit = func(name string) error {
		if visiting[name] {
			return fmt.Errorf("%w: cycle at %q", ErrComputedField, name)
		}
		if visited[name] {
			return nil
		}
		field, ok := collection.Fields.GetByName(name).(*ComputedField)
		if !ok {
			return nil
		}
		visiting[name] = true
		for _, dep := range list.ToUniqueStringSlice(field.Deps) {
			if _, isComputed := collection.Fields.GetByName(dep).(*ComputedField); isComputed {
				if err := visit(dep); err != nil {
					return err
				}
			}
		}
		delete(visiting, name)
		visited[name] = true
		result = append(result, field)
		return nil
	}

	for name := range requested {
		if err := visit(name); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func computedRecordValue(record *Record, allowedDeps map[string]bool, requestInfo *RequestInfo, active map[string]bool) map[string]any {
	export := map[string]any{}
	collection := record.Collection()
	for _, f := range collection.Fields {
		name := f.GetName()
		if _, isComputed := f.(*ComputedField); isComputed {
			continue
		}
		if len(allowedDeps) == 0 || allowedDeps[name] {
			export[name] = safeComputedRecordField(record, name)
		}
	}
	export[FieldNameId] = record.Id
	return export
}

func safeComputedRecordField(record *Record, name string) any {
	// Get() may invoke custom getters; raw values provide stable JSON types.
	v := record.GetRaw(name)
	switch x := v.(type) {
	case types.DateTime:
		return x.String()
	case types.JSONRaw:
		var out any
		_ = json.Unmarshal([]byte(x), &out)
		return out
	default:
		return x
	}
}

func computedRelationValue(app App, record *Record, path string, requestInfo *RequestInfo, field *ComputedField, active map[string]bool) (any, error) {
	if app == nil {
		return nil, fmt.Errorf("relation(%q) requires an application context", path)
	}
	parts := strings.Split(path, ".")
	if len(parts) == 0 || len(parts) > field.normalizedMaxDepth() {
		return nil, fmt.Errorf("invalid relation path %q", path)
	}
	allowed := map[string]bool{}
	for _, rel := range field.Relations {
		allowed[rel] = true
	}
	if !allowed[path] {
		return nil, fmt.Errorf("relation %q is not declared", path)
	}

	current := []*Record{record}
	relationFields := make([]*RelationField, 0, len(parts))
	visitedEdges := map[string]bool{}
	for _, part := range parts {
		if len(current) == 0 {
			return []any{}, nil
		}
		collection := current[0].Collection()
		edge := collection.Id + "." + part
		if visitedEdges[edge] {
			return nil, fmt.Errorf("circular relation path %q", path)
		}
		visitedEdges[edge] = true
		rel, ok := collection.Fields.GetByName(part).(*RelationField)
		if !ok {
			return nil, fmt.Errorf("%q is not a relation field", part)
		}

		idSet := map[string]struct{}{}
		ids := []string{}
		for _, rec := range current {
			for _, id := range rec.GetStringSlice(part) {
				if _, ok := idSet[id]; !ok {
					idSet[id] = struct{}{}
					ids = append(ids, id)
				}
			}
		}
		relCollection, err := app.FindCachedCollectionByNameOrId(rel.CollectionId)
		if err != nil {
			return nil, err
		}
		fetched, err := app.FindRecordsByIds(relCollection.Id, ids)
		if err != nil {
			return nil, err
		}
		fetched = filterComputedRelations(app, fetched, requestInfo)
		relationFields = append(relationFields, rel)
		current = fetched
	}

	values := make([]any, 0, len(current))
	for _, relRecord := range current {
		relRecord.BindComputedContext(app, requestInfo)
		visibleComputed := visibleComputedFieldNames(relRecord.Collection(), requestInfo, app)
		if len(visibleComputed) > 0 {
			if _, err := PrepareComputedFields(context.Background(), app, requestInfo, []*Record{relRecord}, visibleComputed, mapKeys(active)...); err != nil {
				return nil, err
			}
		}
		values = append(values, computedPublicRecordValue(relRecord, requestInfo))
	}
	allMultiple := true
	for _, rel := range relationFields {
		if !rel.IsMultiple() {
			allMultiple = false
		}
	}
	if !allMultiple {
		if len(values) == 0 {
			return nil, nil
		}
		return values[0], nil
	}
	return values, nil
}

func mapKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		if m[k] {
			out = append(out, k)
		}
	}
	return out
}

func visibleComputedFieldNames(collection *Collection, requestInfo *RequestInfo, app App) []string {
	out := []string{}
	for _, f := range collection.Fields {
		c, ok := f.(*ComputedField)
		if !ok {
			continue
		}
		if !c.Hidden || (requestInfo != nil && requestInfo.HasSuperuserAuth()) {
			out = append(out, c.Name)
		}
	}
	return out
}

// PrepareVisibleComputedFields evaluates every visible computed field of the
// provided root records and all currently expanded records. It is intended to
// be called after relation expansion so script relation() results and exported
// expand payloads share one evaluation/error boundary.
func PrepareVisibleComputedFields(app App, requestInfo *RequestInfo, roots []*Record) error {
	visited := map[*Record]bool{}
	var walk func(record *Record) error
	walk = func(record *Record) error {
		if record == nil || visited[record] {
			return nil
		}
		visited[record] = true

		names := visibleComputedFieldNames(record.Collection(), requestInfo, app)
		if len(names) > 0 {
			if _, err := PrepareComputedFields(context.Background(), app, requestInfo, []*Record{record}, names); err != nil {
				return err
			}
		}

		for _, value := range record.Expand() {
			switch rel := value.(type) {
			case *Record:
				if err := walk(rel); err != nil {
					return err
				}
			case []*Record:
				for _, nested := range rel {
					if err := walk(nested); err != nil {
						return err
					}
				}
			}
		}
		return nil
	}

	for _, root := range roots {
		if err := walk(root); err != nil {
			return err
		}
	}
	return nil
}

func computedPublicRecordValue(record *Record, requestInfo *RequestInfo) map[string]any {
	// Use PublicExport after visibility has been resolved. It never exposes
	// password/tokenKey and honors custom Hide/Unhide state.
	if requestInfo != nil && requestInfo.HasSuperuserAuth() {
		record.Unhide(record.Collection().Fields.FieldNames()...)
	}
	record.IgnoreEmailVisibility(requestInfo != nil && requestInfo.HasSuperuserAuth())
	return record.PublicExport()
}

func filterComputedRelations(app App, records []*Record, requestInfo *RequestInfo) []*Record {
	if requestInfo == nil || requestInfo.HasSuperuserAuth() {
		return records
	}
	out := make([]*Record, 0, len(records))
	for _, record := range records {
		if ok, _ := app.CanAccessRecord(record, requestInfo, record.Collection().ViewRule); ok {
			out = append(out, record)
		}
	}
	return out
}
