package core

import (
	"context"
	"regexp"
	"slices"
	"strings"

	validation "github.com/pocketbase/ozzo-validation/v4"
)

func init() {
	Fields[FieldTypeComputed] = func() Field {
		return &ComputedField{}
	}
}

const FieldTypeComputed = "computed"

var (
	_ Field        = (*ComputedField)(nil)
	_ SetterFinder = (*ComputedField)(nil)
	_ GetterFinder = (*ComputedField)(nil)
	_ VirtualField = (*ComputedField)(nil)
)

const (
	// ComputedModeExpression is the safe single-expression mode (eg. `doc.foo + ' ' + doc.bar`).
	ComputedModeExpression = "expression"

	// ComputedModeFunction is the restricted function body mode
	// (eg. `return doc.label + ' (' + doc.$rel('customer')[0]?.name + ')'`).
	ComputedModeFunction = "function"

	// computedDefaultTimeoutMs is the default per-record evaluation timeout in milliseconds.
	computedDefaultTimeoutMs = 100
	// computedMaxTimeoutMs is the hard upper cap of the per-record evaluation timeout.
	computedMaxTimeoutMs = 1000
	// computedMaxExpressionLength is the max allowed script/expression length.
	computedMaxExpressionLength = 10000
)

var computedFieldNameRegex = regexp.MustCompile(`^[a-zA-Z_$][\w$]*$`)

// ComputedField defines a read-only virtual ("computed") field whose value is
// evaluated at read time by a restricted JavaScript runtime (goja).
//
// The value is never persisted in the database and the field doesn't create
// a SQL column. It is populated only when records are enriched (list/view APIs,
// expand loading, realtime events) and is also programmatically available via
// [App.ComputedResolve].
//
// Two modes are supported:
//
//   - "expression": a single safe JS expression evaluated as `(doc, ctx) => (<expression>)`.
//   - "function": a restricted synchronous function body evaluated as
//     `function(doc, ctx) { <script> }`. Only whitelisted JS builtins are available
//     and there are no IO/database/network bindings.
//
// The field itself can still be marked as Hidden and follows the standard
// visibility rules. Additionally, if any of the DependsOn fields is hidden
// (or inaccessible to the current request auth), the computed value is also
// hidden automatically so that hidden data cannot be leaked through it.
type ComputedField struct {
	// Name (required) is the unique name of the field.
	Name string `form:"name" json:"name"`

	// Id is the unique stable field identifier.
	Id string `form:"id" json:"id"`

	// System prevents the renaming and removal of the field.
	System bool `form:"system" json:"system"`

	// Hidden hides the field from the API response.
	Hidden bool `form:"hidden" json:"hidden"`

	// ---

	// Help is an extra text explaining what the field is about.
	Help string `form:"help" json:"help"`

	// Presentable hints the Dashboard UI to use the field value in relation previews.
	Presentable bool `form:"presentable" json:"presentable"`

	// Mode specifies the JS evaluation mode - ComputedModeExpression (default)
	// or ComputedModeFunction.
	Mode string `form:"mode" json:"mode"`

	// Expression is the JS expression or the restricted function body.
	Expression string `form:"expression" json:"expression"`

	// DependsOn is an explicit list of field names (of the same collection) that
	// the expression relies on.
	//
	// It is used for hidden-field propagation, cycle detection and cache invalidation.
	// Relation field dependencies also allow accessing the related record(s)
	// through `doc.$rel("fieldName")` in function mode.
	DependsOn []string `form:"dependsOn" json:"dependsOn"`

	// ResultType optionally hints the expected result type used by the Dashboard UI
	// and for basic output validation: "text" (default), "number", "bool", "json".
	ResultType string `form:"resultType" json:"resultType"`

	// TimeoutMs is the max evaluation time in milliseconds per record.
	//
	// If zero, computedDefaultTimeoutMs is used. Cannot exceed computedMaxTimeoutMs.
	TimeoutMs int `form:"timeoutMs" json:"timeoutMs"`
}

// Type implements [Field.Type].
func (f *ComputedField) Type() string { return FieldTypeComputed }

// GetId implements [Field.GetId].
func (f *ComputedField) GetId() string { return f.Id }

// SetId implements [Field.SetId].
func (f *ComputedField) SetId(id string) { f.Id = id }

// GetName implements [Field.GetName].
func (f *ComputedField) GetName() string { return f.Name }

// SetName implements [Field.SetName].
func (f *ComputedField) SetName(name string) { f.Name = name }

// GetSystem implements [Field.GetSystem].
func (f *ComputedField) GetSystem() bool { return f.System }

// SetSystem implements [Field.SetSystem].
func (f *ComputedField) SetSystem(system bool) { f.System = system }

// GetHidden implements [Field.GetHidden].
func (f *ComputedField) GetHidden() bool { return f.Hidden }

// SetHidden implements [Field.SetHidden].
func (f *ComputedField) SetHidden(hidden bool) { f.Hidden = hidden }

// ColumnType implements [Field.ColumnType].
//
// Computed fields are not persisted - an empty column type is returned and
// the table sync logic skips them (see [VirtualField]).
func (f *ComputedField) ColumnType(_ App) string { return "" }

// IsVirtual implements [VirtualField].
func (f *ComputedField) IsVirtual() bool { return true }

// PrepareValue implements [Field.PrepareValue].
//
// Computed values are not stored in the record's base state; nil is used as
// a placeholder until the record is enriched and the value is evaluated.
func (f *ComputedField) PrepareValue(_ *Record, raw any) (any, error) {
	return nil, nil
}

// ValidateValue implements [Field.ValidateValue].
//
// Computed fields are read-only; submitted values are dropped by FindSetter
// (no-op setter), so there is nothing to persist or validate here.
func (f *ComputedField) ValidateValue(_ context.Context, _ App, record *Record) error {
	return nil
}

// ValidateSettings implements [Field.ValidateSettings].
func (f *ComputedField) ValidateSettings(ctx context.Context, app App, collection *Collection) error {
	f.NormalizeOptions()

	return validation.ValidateStruct(f,
		validation.Field(&f.Id, validation.By(DefaultFieldIdValidationRule)),
		validation.Field(&f.Name, validation.By(DefaultFieldNameValidationRule)),
		validation.Field(&f.Help, validation.By(DefaultFieldHelpValidationRule)),
		validation.Field(&f.Mode, validation.In(ComputedModeExpression, ComputedModeFunction)),
		validation.Field(&f.Expression,
			validation.Required.Error("The expression is required."),
			validation.Length(1, computedMaxExpressionLength),
			validation.By(f.checkCompiles(app)),
		),
		validation.Field(&f.ResultType,
			validation.In("", "text", "number", "bool", "json"),
		),
		validation.Field(&f.TimeoutMs,
			validation.Min(0),
			validation.Max(computedMaxTimeoutMs),
		),
		validation.Field(&f.DependsOn, validation.By(f.checkDependencies(collection))),
	)
}

// NormalizeOptions fills the defaults of the computed field options.
func (f *ComputedField) NormalizeOptions() {
	if f.Mode == "" {
		f.Mode = ComputedModeExpression
	}
	if f.ResultType == "" {
		f.ResultType = "text"
	}
	if f.TimeoutMs <= 0 {
		f.TimeoutMs = computedDefaultTimeoutMs
	}
	f.DependsOn = normalizeDependsOn(f.DependsOn)
}

func normalizeDependsOn(deps []string) []string {
	result := make([]string, 0, len(deps))
	for _, d := range deps {
		d = strings.TrimSpace(d)
		if d != "" && !slices.Contains(result, d) {
			result = append(result, d)
		}
	}
	return result
}

func (f *ComputedField) checkCompiles(app App) validation.RuleFunc {
	return func(value any) error {
		v, _ := value.(string)
		if strings.TrimSpace(v) == "" {
			return nil // handled by Required
		}
		f.NormalizeOptions()
		if err := app.ComputedEngine().Compile(f); err != nil {
			return validation.NewError("validation_computed_compile", "Invalid JavaScript expression.").
				SetParams(map[string]any{"error": err.Error()})
		}
		return nil
	}
}

func (f *ComputedField) checkDependencies(collection *Collection) validation.RuleFunc {
	return func(value any) error {
		errs := validation.Errors{}

		for _, dep := range f.DependsOn {
			if !computedFieldNameRegex.MatchString(dep) {
				errs[dep] = validation.NewError("validation_invalid_computed_dependency", "Invalid dependency field name.")
				continue
			}
			if dep == f.Name {
				errs[dep] = validation.NewError("validation_computed_self_dependency", "A computed field cannot depend on itself.")
				continue
			}
			if collection.Fields.GetByName(dep) == nil {
				errs[dep] = validation.NewError("validation_unknown_computed_dependency", "Unknown or missing dependency field.")
				continue
			}
		}

		if len(errs) > 0 {
			return errs
		}

		// recursive/cyclic dependency check across all computed fields in the collection
		return checkComputedDependenciesCycle(collection)
	}
}

// checkComputedDependenciesCycle ensures that there is no cycle in the
// computed -> computed dependency graph of the collection.
func checkComputedDependenciesCycle(collection *Collection) error {
	computed := map[string]*ComputedField{}
	for _, fld := range collection.Fields {
		if cf, ok := fld.(*ComputedField); ok {
			cf.NormalizeOptions()
			computed[cf.Name] = cf
		}
	}

	// DFS coloring: 0 = unvisited, 1 = in progress, 2 = done
	state := map[string]int{}

	var visit func(name string, trail []string) error
	visit = func(name string, trail []string) error {
		switch state[name] {
		case 1:
			cycleStart := slices.Index(trail, name)
			cycle := append(trail[cycleStart:], name)
			return validation.NewError(
				"validation_computed_dependency_cycle",
				"Cyclic computed field dependency detected: "+strings.Join(cycle, " -> "),
			)
		case 2:
			return nil
		}

		state[name] = 1
		trail = append(trail, name)

		if cf, ok := computed[name]; ok {
			for _, dep := range cf.DependsOn {
				if _, isComputed := computed[dep]; isComputed {
					if err := visit(dep, trail); err != nil {
						return err
					}
				}
			}
		}

		state[name] = 2
		return nil
	}

	// deterministic order
	names := make([]string, 0, len(computed))
	for name := range computed {
		names = append(names, name)
	}
	slices.Sort(names)

	for _, name := range names {
		if err := visit(name, nil); err != nil {
			return err
		}
	}

	return nil
}

// FindSetter implements [SetterFinder].
//
// Computed fields are read-only - the setter is a no-op so that stray
// submitted values (eg. via API) are silently ignored instead of being
// persisted into the custom/unknown data store.
func (f *ComputedField) FindSetter(key string) SetterFunc {
	if key == f.Name {
		return noopSetter
	}
	return nil
}

// FindGetter implements [GetterFinder].
//
// The getter returns the evaluated value if it was already computed for the
// record (eg. during enrich), otherwise nil (computed fields are lazy and
// require an evaluation context).
func (f *ComputedField) FindGetter(key string) GetterFunc {
	if key != f.Name {
		return nil
	}
	return func(record *Record) any {
		if v, evaluated, _ := record.computedStoreGet(f.Name); evaluated {
			return v
		}
		return nil
	}
}

// GetTimeout returns the effective evaluation timeout.
func (f *ComputedField) GetTimeout() int {
	if f.TimeoutMs <= 0 {
		return computedDefaultTimeoutMs
	}
	if f.TimeoutMs > computedMaxTimeoutMs {
		return computedMaxTimeoutMs
	}
	return f.TimeoutMs
}

// IsExpression reports whether the field uses the single expression mode.
func (f *ComputedField) IsExpression() bool {
	f.NormalizeOptions()
	return f.Mode == ComputedModeExpression
}

// computedValueMarker wraps an evaluated computed value so it can be stored
// in the record data without being treated as a user-submitted value.
type computedValueMarker struct {
	value any
}

// ComputedFieldsOf returns all computed fields from the collection (in declaration order).
func ComputedFieldsOf(collection *Collection) []*ComputedField {
	if collection == nil {
		return nil
	}
	result := make([]*ComputedField, 0)
	for _, fld := range collection.Fields {
		if cf, ok := fld.(*ComputedField); ok {
			cf.NormalizeOptions()
			result = append(result, cf)
		}
	}
	return result
}
