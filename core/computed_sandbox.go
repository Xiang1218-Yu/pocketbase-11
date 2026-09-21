package core

import (
	"encoding/json/v2"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/dop251/goja"
	"github.com/spf13/cast"
)

const sandboxInstanceKey = "__pbComputedSandbox"

// computedSandbox wraps a single goja runtime configured as a restricted
// evaluation environment for computed field scripts.
//
// The sandbox exposes ONLY:
//   - `doc`    - the current record fields (plus doc.$rel/relation accessors)
//   - `ctx`    - request-scoped context (auth snapshot, query, expand, data)
//
// There is no access to $app, the database, files, network, timers, require,
// process, console (aside from a no-op stub) or the Function/eval constructors.
type computedSandbox struct {
	vm *goja.Runtime

	docValue goja.Value
	ctxValue goja.Value

	// bound per evaluation
	req    *ComputeRequest
	record *Record
	field  *ComputedField
}

// newSandboxVM creates a fresh locked-down goja runtime.
func (e *computedEngine) newSandboxVM() *goja.Runtime {
	vm := goja.New()

	// disable/neuter dangerous globals before user code can run
	lockdownSandbox(vm)

	sb := &computedSandbox{vm: vm}
	_ = vm.Set(sandboxInstanceKey, sb)

	return vm
}

// lockdownSandbox removes or replaces globals that could be used to escape
// the sandbox or cause side effects.
func lockdownSandbox(vm *goja.Runtime) {
	forbidden := func(name string) {
		_ = vm.Set(name, vm.ToValue(func(call goja.FunctionCall) goja.Value {
			panic(vm.NewTypeError("%s is not available in computed fields", name))
		}))
	}

	// code generation / reflection
	forbidden("eval")
	forbidden("Function")

	// event loop / IO
	forbidden("setTimeout")
	forbidden("setInterval")
	forbidden("setImmediate")
	forbidden("clearTimeout")
	forbidden("clearInterval")
	forbidden("clearImmediate")
	forbidden("requestAnimationFrame")
	forbidden("queueMicrotask")

	// modules & process
	forbidden("require")
	_ = vm.Set("process", goja.Undefined())
	_ = vm.Set("globalThis", vm.GlobalObject())
	_ = vm.Set("global", vm.GlobalObject())
	_ = vm.Set("window", goja.Undefined())
	_ = vm.Set("fetch", goja.Undefined())
	_ = vm.Set("XMLHttpRequest", goja.Undefined())
	_ = vm.Set("WebSocket", goja.Undefined())
	_ = vm.Set("importScripts", goja.Undefined())

	// no-op logging stub (scripts may use console.log for debugging but it must
	// never leak server internals or perform IO)
	consoleObj := vm.NewObject()
	_ = consoleObj.Set("log", vm.ToValue(func(goja.FunctionCall) goja.Value { return goja.Undefined() }))
	_ = consoleObj.Set("info", consoleObj.Get("log"))
	_ = consoleObj.Set("warn", consoleObj.Get("log"))
	_ = consoleObj.Set("error", consoleObj.Get("log"))
	_ = consoleObj.Set("debug", consoleObj.Get("log"))
	_ = vm.Set("console", consoleObj)
}

// bind prepares the sandbox for evaluating the given field on the given record.
func (sb *computedSandbox) bind(req *ComputeRequest, record *Record, field *ComputedField, program *goja.Program) error {
	sb.req = req
	sb.record = record
	sb.field = field

	// run the compiled program in the sandbox to obtain the evaluator function
	// (the program is a self-contained function expression, not touching globals)
	fnVal, err := sb.vm.RunProgram(program)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrComputedScript, err)
	}

	if err := sb.vm.Set("__pb_computed_run", fnVal); err != nil {
		return err
	}

	sb.docValue = sb.buildRecordObject(record, true, 0)
	sb.ctxValue = sb.buildCtxObject()

	return nil
}

// buildCtxObject constructs the `ctx` object exposed to scripts.
func (sb *computedSandbox) buildCtxObject() goja.Value {
	obj := sb.vm.NewObject()

	info := sb.req.config.RequestInfo

	// ctx.data
	_ = obj.Set("data", sb.vm.ToValue(sb.req.config.ContextData))

	// ctx.now - stable ISO timestamp per evaluation (no direct Date constructor access)
	_ = obj.Set("now", sb.vm.ToValue(time.Now().UTC().Format(time.RFC3339Nano)))

	if info == nil {
		_ = obj.Set("auth", goja.Null())
		_ = obj.Set("query", sb.vm.ToValue(map[string]string{}))
		_ = obj.Set("headers", sb.vm.ToValue(map[string]string{}))
		_ = obj.Set("method", "")
		_ = obj.Set("context", "")
		return obj
	}

	// ctx.auth: snapshot of the PUBLIC auth record (already unhidden by the
	// record field resolver when constructed) - safe plain object
	if info.Auth != nil {
		_ = obj.Set("auth", sb.vm.ToValue(sb.safeRecordExport(info.Auth)))
	} else {
		_ = obj.Set("auth", goja.Null())
	}

	_ = obj.Set("query", sb.vm.ToValue(info.Query))
	_ = obj.Set("headers", sb.vm.ToValue(info.Headers))
	_ = obj.Set("method", sb.vm.ToValue(info.Method))
	_ = obj.Set("context", sb.vm.ToValue(info.Context))

	return obj
}

// safeRecordExport returns a JSON-safe snapshot of a record, honoring its
// visibility flags (PublicExport).
func (sb *computedSandbox) safeRecordExport(record *Record) any {
	if record == nil {
		return nil
	}
	return record.PublicExport()
}

// buildRecordObject constructs the JS object representing a record.
//
// root=true marks the main record of the evaluation and enables the $expand
// accessor (returns records already loaded via the API `expand` mechanism).
func (sb *computedSandbox) buildRecordObject(record *Record, root bool, depth int) goja.Value {
	if record == nil {
		return goja.Null()
	}

	obj := sb.vm.NewObject()

	collection := record.Collection()

	// expose the record's fields to the script.
	for _, f := range collection.Fields {
		name := f.GetName()

		if _, isComputed := f.(*ComputedField); isComputed {
			// computed fields are exposed only when they are declared
			// dependencies and were already evaluated in topological order
			// (evaluated values are plain JSON-safe data, no script nesting)
			if !slices.Contains(sb.field.DependsOn, name) {
				continue
			}
			if v, evaluated, hasValue := record.computedStoreGet(name); evaluated && hasValue {
				_ = obj.Set(name, sb.vm.ToValue(v))
			}
			continue
		}

		if !sb.fieldAccessibleForRecord(record, f) {
			continue
		}
		raw := record.GetRaw(name)
		_ = obj.Set(name, sb.vm.ToValue(jsSafeValue(raw)))
	}

	// $rel(name) -> related record(s) of the root/current record (only declared deps)
	_ = obj.Set("$rel", sb.vm.ToValue(func(call goja.FunctionCall) goja.Value {
		if len(call.Arguments) == 0 {
			panic(sb.vm.NewTypeError("$rel requires a relation field name"))
		}
		name := call.Argument(0).String()
		rels, relField, err := sb.loadRelations(record, name, depth)
		if err != nil {
			panic(sb.vm.NewGoError(err))
		}
		return sb.recordsToJs(rels, relField, depth)
	}))

	if root {
		// $expand(name) -> already-expanded records (loaded by the API ?expand mechanism)
		_ = obj.Set("$expand", sb.vm.ToValue(func(call goja.FunctionCall) goja.Value {
			if len(call.Arguments) == 0 {
				panic(sb.vm.NewTypeError("$expand requires a relation field name"))
			}
			name := call.Argument(0).String()
			expanded := record.Expand()
			v, ok := expanded[name]
			if !ok || v == nil {
				return goja.Null()
			}
			switch x := v.(type) {
			case *Record:
				return sb.buildRecordObject(x, false, depth+1)
			case []*Record:
				relField, _ := record.Collection().Fields.GetByName(name).(*RelationField)
				return sb.recordsToJs(x, relField, depth)
			default:
				return goja.Null()
			}
		}))
	}

	return obj
}

// recordsToJs converts a relation records slice to a single record object or
// an array of objects depending on the relation's MaxSelect.
func (sb *computedSandbox) recordsToJs(records []*Record, relField *RelationField, depth int) goja.Value {
	multiple := relField != nil && relField.IsMultiple()

	values := make([]goja.Value, 0, len(records))
	for _, r := range records {
		values = append(values, sb.buildRecordObject(r, false, depth+1))
	}

	if !multiple {
		if len(values) == 0 {
			return goja.Null()
		}
		return values[0]
	}

	arr := sb.vm.NewArray(len(values))
	for i, v := range values {
		if err := arr.Set(strconv.Itoa(i), v); err != nil {
			panic(sb.vm.NewGoError(err))
		}
	}
	return arr
}

// loadRelations fetches the related records of `record` for the given relation
// field, enforcing dependency declaration, depth and cycle restrictions.
func (sb *computedSandbox) loadRelations(record *Record, fieldName string, depth int) ([]*Record, *RelationField, error) {
	if depth >= computedMaxRelationDepth {
		return nil, nil, fmt.Errorf("%w: max relation depth %d exceeded", ErrComputedCycle, computedMaxRelationDepth)
	}

	relField, _ := record.Collection().Fields.GetByName(fieldName).(*RelationField)
	if relField == nil {
		return nil, nil, fmt.Errorf("%w: %q is not a relation field of %q", ErrComputedForbidden, fieldName, record.Collection().Name)
	}

	// the relation field must be declared as a dependency of the computed field
	declared := false
	for _, dep := range sb.field.DependsOn {
		if dep == fieldName {
			declared = true
			break
		}
	}
	if !declared {
		return nil, nil, fmt.Errorf("%w: relation %q must be declared in dependsOn", ErrComputedForbidden, fieldName)
	}

	// the relation field must be accessible (hidden propagation)
	if !sb.fieldAccessibleForRecord(record, relField) {
		return nil, relField, nil
	}

	rels, err := sb.req.getRelations(record, relField)
	return rels, relField, err
}

// getRelations returns the cached related records or fetches them through the
// request relation fetcher.
func (req *ComputeRequest) getRelations(record *Record, field *RelationField) ([]*Record, error) {
	cacheKey := field.CollectionId + ":" + record.Id + ":" + field.Name

	req.relationMu.Lock()
	if cached, ok := req.relCache[cacheKey]; ok {
		req.relationMu.Unlock()
		return cached, nil
	}
	if req.inFlight[cacheKey] {
		req.relationMu.Unlock()
		return nil, fmt.Errorf("%w: %s on %s", ErrComputedCycle, field.Name, record.Id)
	}
	req.inFlight[cacheKey] = true
	req.relationMu.Unlock()

	rels, err := req.config.RelationFetcher(record, field)

	req.relationMu.Lock()
	delete(req.inFlight, cacheKey)
	if err == nil {
		req.relCache[cacheKey] = rels
	}
	req.relationMu.Unlock()

	return rels, err
}

// fieldAccessibleForRecord reports whether the field of the record's collection
// is visible for the current request (superuser / public).
func (sb *computedSandbox) fieldAccessibleForRecord(record *Record, f Field) bool {
	info := sb.req.config.RequestInfo

	// server-side usage without request context: full access
	if info == nil {
		return true
	}
	if info.HasSuperuserAuth() {
		return true
	}
	if f.GetHidden() {
		return false
	}
	// per-record visibility overrides (Hide/Unhide)
	if visible, hasOverride := record.customVisibility.GetOk(f.GetName()); hasOverride {
		return visible
	}
	return true
}

// jsSafeValue converts record raw values into JSON-safe Go values suitable for
// goja export.
func jsSafeValue(v any) any {
	if v == nil {
		return nil
	}
	switch x := v.(type) {
	case []byte:
		return string(x)
	case time.Time:
		return x.UTC().Format(time.RFC3339Nano)
	default:
		// types.DateTime, types.JSONRaw, JSONArray, etc. marshal through JSON
		if b, err := json.Marshal(v); err == nil {
			var out any
			if err := json.Unmarshal(b, &out); err == nil {
				return out
			}
		}
		return v
	}
}

// validateComputedResult verifies that the produced value is representable and
// matches the declared result type.
func validateComputedResult(v any, field *ComputedField) error {
	if v == nil {
		return nil
	}

	// size guard
	encoded, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("%w: value is not JSON serializable: %v", ErrComputedResult, err)
	}
	if len(encoded) > computedMaxResultJSONLength {
		return fmt.Errorf("%w: result exceeds the max size of %d bytes", ErrComputedResult, computedMaxResultJSONLength)
	}

	switch field.ResultType {
	case "number":
		if _, err := cast.ToFloat64E(v); err != nil {
			return fmt.Errorf("%w: expected number, got %T", ErrComputedResult, v)
		}
	case "bool":
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("%w: expected bool, got %T", ErrComputedResult, v)
		}
	case "text":
		switch v.(type) {
		case string, fmt.Stringer:
			// ok
		default:
			return fmt.Errorf("%w: expected text, got %T", ErrComputedResult, v)
		}
	case "json":
		// already validated as JSON serializable above
	}

	return nil
}
