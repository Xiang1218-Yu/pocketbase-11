package core_test

import (
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
)

func newComputedTestCollection() *core.Collection {
	c := core.NewBaseCollection("computed_test")
	c.Fields.Add(
		&core.TextField{Name: "title"},
		&core.NumberField{Name: "qty"},
		&core.TextField{Name: "secret", Hidden: true},
		&core.ComputedField{
			Name:       "label",
			Mode:       core.ComputedModeExpression,
			Expression: "doc.title + ' x' + doc.qty",
			DependsOn:  []string{"title", "qty"},
			ResultType: "text",
		},
		&core.ComputedField{
			Name:       "upper",
			Mode:       core.ComputedModeFunction,
			Expression: "return doc.label.toUpperCase()",
			DependsOn:  []string{"label"},
			ResultType: "text",
		},
	)
	return c
}

func TestComputedFieldBasicResolve(t *testing.T) {
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()

	collection := newComputedTestCollection()
	if err := app.Save(collection); err != nil {
		t.Fatal(err)
	}
	defer app.Delete(collection)

	record := core.NewRecord(collection)
	record.Set("title", "apple")
	record.Set("qty", 3)

	if err := app.ComputedResolveOne(record); err != nil {
		t.Fatalf("ComputedResolveOne error: %v", err)
	}

	if got := record.GetString("label"); got != "apple x3" {
		t.Fatalf("expected label %q, got %q", "apple x3", got)
	}
	if got := record.GetString("upper"); got != "APPLE X3" {
		t.Fatalf("expected upper %q, got %q", "APPLE X3", got)
	}
}

func TestComputedFieldIsReadOnly(t *testing.T) {
	collection := newComputedTestCollection()
	record := core.NewRecord(collection)

	record.Set("label", "hacked")

	if got := record.GetRaw("label"); got != nil {
		t.Fatalf("computed field writes must be dropped, got %#v", got)
	}
}

func TestComputedFieldNoDBColumnOrExport(t *testing.T) {
	collection := newComputedTestCollection()

	f := collection.Fields.GetByName("label")
	cf, ok := f.(*core.ComputedField)
	if !ok {
		t.Fatal("label is not a computed field")
	}
	if ct := cf.ColumnType(nil); ct != "" {
		t.Fatalf("computed fields must not define a column, got %q", ct)
	}

	record := core.NewRecord(collection)
	record.Set("title", "x")

	export, err := record.DBExport(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := export["label"]; exists {
		t.Fatalf("computed fields must not be exported to the DB, got %#v", export)
	}
}

func TestComputedFieldCycleValidation(t *testing.T) {
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()

	c := core.NewBaseCollection("computed_cycle")
	c.Fields.Add(
		&core.ComputedField{Name: "a", Mode: "expression", Expression: "doc.b", DependsOn: []string{"b"}, ResultType: "text"},
		&core.ComputedField{Name: "b", Mode: "expression", Expression: "doc.a", DependsOn: []string{"a"}, ResultType: "text"},
	)

	err = c.Fields.GetByName("a").ValidateSettings(nil, app, c)
	if err == nil {
		t.Fatal("expected cyclic dependency validation error, got nil")
	}
}

func TestComputedFieldSelfDependencyValidation(t *testing.T) {
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()

	c := core.NewBaseCollection("computed_self")
	c.Fields.Add(
		&core.ComputedField{Name: "a", Mode: "expression", Expression: "doc.a", DependsOn: []string{"a"}, ResultType: "text"},
	)

	if err := c.Fields.GetByName("a").ValidateSettings(nil, app, c); err == nil {
		t.Fatal("expected self-dependency validation error, got nil")
	}
}

func TestComputedFieldUnknownDependencyValidation(t *testing.T) {
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()

	c := core.NewBaseCollection("computed_unknown_dep")
	c.Fields.Add(
		&core.ComputedField{Name: "a", Mode: "expression", Expression: "doc.missing", DependsOn: []string{"missing"}, ResultType: "text"},
	)

	if err := c.Fields.GetByName("a").ValidateSettings(nil, app, c); err == nil {
		t.Fatal("expected unknown dependency validation error, got nil")
	}
}

func TestComputedFieldInvalidSyntaxValidation(t *testing.T) {
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()

	c := core.NewBaseCollection("computed_bad_js")
	c.Fields.Add(
		&core.ComputedField{Name: "a", Mode: "expression", Expression: "doc.", ResultType: "text"},
	)

	if err := c.Fields.GetByName("a").ValidateSettings(nil, app, c); err == nil {
		t.Fatal("expected JS compile validation error, got nil")
	}
}

func TestComputedFieldForbiddenGlobals(t *testing.T) {
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()

	collection := core.NewBaseCollection("computed_forbidden")
	collection.Fields.Add(
		&core.ComputedField{
			Name:       "evil",
			Mode:       core.ComputedModeFunction,
			Expression: "return Function('return 1')()",
			ResultType: "json",
		},
	)
	if err := app.Save(collection); err != nil {
		t.Fatal(err)
	}
	defer app.Delete(collection)

	record := core.NewRecord(collection)
	if err := app.ComputedResolveOne(record); err == nil {
		t.Fatal("expected forbidden Function constructor error, got nil")
	}
}

func TestComputedFieldTimeout(t *testing.T) {
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()

	collection := core.NewBaseCollection("computed_timeout")
	collection.Fields.Add(
		&core.ComputedField{
			Name:       "slow",
			Mode:       core.ComputedModeFunction,
			Expression: "while(true){}",
			TimeoutMs:  20,
			ResultType: "json",
		},
	)
	if err := app.Save(collection); err != nil {
		t.Fatal(err)
	}
	defer app.Delete(collection)

	record := core.NewRecord(collection)

	start := time.Now()
	err = app.ComputedResolveOne(record)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("timeout was not enforced in time, took %s", elapsed)
	}
}

func TestComputedFieldHiddenDependencyPropagation(t *testing.T) {
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()

	collection := core.NewBaseCollection("computed_hidden_dep")
	collection.Fields.Add(
		&core.TextField{Name: "secret", Hidden: true},
		&core.ComputedField{
			Name:       "leak",
			Mode:       core.ComputedModeExpression,
			Expression: "doc.secret",
			DependsOn:  []string{"secret"},
			ResultType: "text",
		},
	)

	record := core.NewRecord(collection)
	record.Set("secret", "shh")

	// guest request (non-superuser)
	req := core.NewComputeRequest(app, core.ComputedRequestConfig{
		RequestInfo: &core.RequestInfo{Method: "GET", Context: core.RequestInfoContextRealtime},
	})
	if err := app.ComputedResolve(req, []*core.Record{record}); err != nil {
		t.Fatal(err)
	}

	exported := record.PublicExport()
	if _, exists := exported["leak"]; exists {
		t.Fatalf("computed field depending on hidden field must be hidden, export: %#v", exported)
	}

	// server-side (no request info): visible and evaluated
	record2 := core.NewRecord(collection)
	record2.Set("secret", "shh")
	if err := app.ComputedResolveOne(record2); err != nil {
		t.Fatal(err)
	}
	if got := record2.GetString("leak"); got != "shh" {
		t.Fatalf("expected server-side value %q, got %q", "shh", got)
	}
}

func TestComputedFilterParser(t *testing.T) {
	f, err := core.ParseComputedFilter("label = 'a x3' || (qty > 2 && upper != 'X')")
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if f.IsEmpty() {
		t.Fatal("expected non-empty filter")
	}

	collection := newComputedTestCollection()
	if !f.ReferencesComputedFields(collection) {
		t.Fatal("expected the filter to reference computed fields")
	}

	sql := f.SQLSafeFilterForTest(collection)
	if sql == "" {
		t.Fatal("expected non-empty SQL filter")
	}
}

func TestComputedFilterEval(t *testing.T) {
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()

	collection := newComputedTestCollection()
	if err := app.Save(collection); err != nil {
		t.Fatal(err)
	}
	defer app.Delete(collection)

	record := core.NewRecord(collection)
	record.Set("title", "apple")
	record.Set("qty", 3)
	if err := app.ComputedResolveOne(record); err != nil {
		t.Fatal(err)
	}

	req := core.NewComputeRequest(app, core.ComputedRequestConfig{})

	f1, _ := core.ParseComputedFilter("label = 'apple x3'")
	if !f1.Eval(record, req) {
		t.Fatal("expected label match")
	}

	f2, _ := core.ParseComputedFilter("qty > 2 && upper = 'APPLE X3'")
	if !f2.Eval(record, req) {
		t.Fatal("expected composite computed+regular match")
	}

	f3, _ := core.ParseComputedFilter("label ~ 'banana'")
	if f3.Eval(record, req) {
		t.Fatal("did not expect like match")
	}
}
