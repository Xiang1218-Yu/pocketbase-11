package core

import (
	"context"
	"strings"

	validation "github.com/pocketbase/ozzo-validation/v4"
	"github.com/pocketbase/pocketbase/tools/list"
)

func init() {
	Fields[FieldTypeComputed] = func() Field {
		return &ComputedField{}
	}
}

const FieldTypeComputed = "computed"

const (
	ComputedModeExpression = "expression"
	ComputedModeFunction   = "function"
)

var (
	_ Field        = (*ComputedField)(nil)
	_ GetterFinder = (*ComputedField)(nil)
	_ SetterFinder = (*ComputedField)(nil)
)

// ComputedField defines a read-only, non-persisted field whose value is
// evaluated with a restricted JavaScript expression or function.
type ComputedField struct {
	Name string `form:"name" json:"name"`
	Id   string `form:"id" json:"id"`

	System bool `form:"system" json:"system"`
	Hidden bool `form:"hidden" json:"hidden"`

	Presentable bool   `form:"presentable" json:"presentable"`
	Help        string `form:"help" json:"help"`

	// Mode is either "expression" (default) or "function".
	Mode string `form:"mode" json:"mode"`

	// Expression is evaluated as `(<expression>)`.
	Expression string `form:"expression" json:"expression"`

	// Function is wrapped as `function(record, relation){ <Function> }`.
	// The function body must explicitly return a value.
	Function string `form:"function" json:"function"`

	// Deps is an explicit allow-list of local field names available to
	// the script. Empty means "all visible local fields" and is retained
	// only for simple expressions. Fields that are hidden from the current
	// auth view must be omitted (a visible computed field may not depend on
	// hidden local fields).
	Deps []string `form:"deps" json:"deps"`

	// Relations is an explicit allow-list of relation paths that the script
	// may load through relation("path"), e.g. "customer" or "customer.country".
	Relations []string `form:"relations" json:"relations"`

	// TimeoutMS interrupts a script after the specified milliseconds.
	// A zero value applies the safe default (100ms).
	TimeoutMS int `form:"timeoutMS" json:"timeoutMS"`

	// MaxDepth limits relation lookups performed by a single evaluation.
	// A zero value applies the safe default (same as maxNestedRels).
	MaxDepth int `form:"maxDepth" json:"maxDepth"`
}

func (f *ComputedField) Type() string { return FieldTypeComputed }
func (f *ComputedField) GetId() string { return f.Id }
func (f *ComputedField) SetId(id string) { f.Id = id }
func (f *ComputedField) GetName() string { return f.Name }
func (f *ComputedField) SetName(name string) { f.Name = name }
func (f *ComputedField) GetSystem() bool { return f.System }
func (f *ComputedField) SetSystem(system bool) { f.System = system }
func (f *ComputedField) GetHidden() bool { return f.Hidden }
func (f *ComputedField) SetHidden(hidden bool) { f.Hidden = hidden }

// ColumnType returns empty string: computed fields are virtual and never
// persisted. Schema sync explicitly skips this type.
func (f *ComputedField) ColumnType(app App) string { return "" }

// PrepareValue returns nil. The value is computed lazily or during record
// enrichment and must not be stored in record state by the field itself.
func (f *ComputedField) PrepareValue(record *Record, raw any) (any, error) {
	return nil, nil
}

func (f *ComputedField) ValidateValue(ctx context.Context, app App, record *Record) error {
	return nil // computed fields cannot be submitted or persisted
}

func (f *ComputedField) ValidateSettings(ctx context.Context, app App, collection *Collection) error {
	mode := f.normalizedMode()
	deps := list.ToUniqueStringSlice(f.Deps)
	rels := list.ToUniqueStringSlice(f.Relations)

	err := validation.ValidateStruct(f,
		validation.Field(&f.Id, validation.By(DefaultFieldIdValidationRule)),
		validation.Field(&f.Name, validation.By(DefaultFieldNameValidationRule)),
		validation.Field(&f.Help, validation.By(DefaultFieldHelpValidationRule)),
		validation.Field(&f.Mode, validation.In("", ComputedModeExpression, ComputedModeFunction)),
		validation.Field(&f.Expression,
			validation.When(mode == ComputedModeExpression, validation.Required, validation.Length(1, 5000)),
			validation.When(mode == ComputedModeFunction, validation.Empty),
		),
		validation.Field(&f.Function,
			validation.When(mode == ComputedModeFunction, validation.Required, validation.Length(1, 10000)),
			validation.When(mode == ComputedModeExpression, validation.Empty),
		),
		validation.Field(&f.TimeoutMS, validation.Min(0), validation.Max(5000)),
		validation.Field(&f.MaxDepth, validation.Min(0), validation.Max(maxNestedRels)),
	)
	if err != nil {
		return err
	}

	for _, dep := range deps {
		depField := collection.Fields.GetByName(dep)
		if depField == nil {
			return validation.NewError("validation_computed_missing_dependency", "Computed dependency %q does not exist.").
				SetParams(map[string]any{"field": dep})
		}
		if dep == f.Name {
			return validation.NewError("validation_computed_self_dependency", "Computed field cannot depend on itself.")
		}
		if _, isComputed := depField.(*ComputedField); isComputed {
			return validation.NewError("validation_computed_dependency", "Computed fields can depend only on persisted fields.").
				SetParams(map[string]any{"field": dep})
		}
		if !f.Hidden && depField.GetHidden() {
			return validation.NewError("validation_computed_hidden_dependency", "A visible computed field cannot depend on hidden field %q.").
				SetParams(map[string]any{"field": dep})
		}
	}

	for _, rel := range rels {
		if err := validateComputedRelationPath(app, collection, strings.Split(rel, "."), !f.Hidden); err != nil {
			return err
		}
	}

	// Validate the script eagerly so invalid schema changes are rejected.
	if _, err := newComputedProgram(f); err != nil {
		return validation.NewError("validation_computed_invalid_script", "Invalid computed script: "+err.Error())
	}

	return nil
}

func (f *ComputedField) normalizedMode() string {
	if f.Mode == ComputedModeFunction {
		return ComputedModeFunction
	}
	return ComputedModeExpression
}

func (f *ComputedField) normalizedTimeout() int {
	if f.TimeoutMS > 0 {
		return f.TimeoutMS
	}
	return 100
}

func (f *ComputedField) normalizedMaxDepth() int {
	if f.MaxDepth > 0 {
		return f.MaxDepth
	}
	return maxNestedRels
}

// FindSetter ignores submitted values. Computed fields are read-only and
// never populated from create/update payloads.
func (f *ComputedField) FindSetter(key string) SetterFunc {
	if key == f.Name {
		return noopSetter
	}
	return nil
}

// FindGetter returns the prepared, in-memory computed value.
//
// Computed values are populated in one batch by [PrepareComputedFields]
// (API list/view/expand/realtime paths do this automatically). Records that
// have not been enriched report nil rather than triggering an implicit,
// context-less script evaluation.
func (f *ComputedField) FindGetter(key string) GetterFunc {
	if key != f.Name {
		return nil
	}

	return func(record *Record) any {
		if record.computed != nil && record.computed.prepared[f.Name] {
			return record.GetRaw(f.Name)
		}
		return nil
	}
}

func validateComputedRelationPath(app App, collection *Collection, parts []string, disallowHidden bool) error {
	if len(parts) == 0 || len(parts) > maxNestedRels {
		return validation.NewError("validation_computed_invalid_relation_path", "Invalid computed relation path.")
	}

	current := collection
	for _, part := range parts {
		field := current.Fields.GetByName(part)
		rel, ok := field.(*RelationField)
		if !ok {
			return validation.NewError("validation_computed_invalid_relation_path", "Computed relation path %q is invalid.").
				SetParams(map[string]any{"path": strings.Join(parts, ".")})
		}
		if disallowHidden && rel.GetHidden() {
			return validation.NewError("validation_computed_hidden_relation_dependency", "A visible computed field cannot depend on hidden relation %q.").
				SetParams(map[string]any{"field": rel.GetName()})
		}

		next, err := app.FindCachedCollectionByNameOrId(rel.CollectionId)
		if err != nil || next == nil {
			return validation.NewError("validation_computed_missing_relation_collection", "Related collection for %q does not exist.").
				SetParams(map[string]any{"field": rel.GetName()})
		}
		if disallowHidden && next.ListRule == nil && next.ViewRule == nil {
			return validation.NewError("validation_computed_relation_not_accessible", "Computed relation %q is not accessible to non-superusers.").
				SetParams(map[string]any{"field": rel.GetName()})
		}
		current = next
	}

	return nil
}

