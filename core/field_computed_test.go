package core_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
)

func TestComputedFieldIsReadOnly(t *testing.T) {
	collection := core.NewBaseCollection("computed_ro")
	collection.Fields.Add(
		&core.TextField{Name: "title"},
		&core.ComputedField{Name: "label", Expression: `record.title`, Deps: []string{"title"}},
	)

	record := core.NewRecord(collection)
	record.Set("title", "hello")
	record.Set("label", "forged")

	if got := record.GetRaw("label"); got != nil {
		t.Fatalf("submitted computed values must be ignored, got %#v", got)
	}
}

func TestComputedFieldExpressionEvaluation(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	collection := core.NewBaseCollection("computed_test")
	collection.Fields.Add(
		&core.TextField{Name: "title"},
		&core.ComputedField{
			Name:       "label",
			Expression: `record.title.toUpperCase() + '!'`,
			Deps:       []string{"title"},
		},
	)

	record := core.NewRecord(collection)
	record.SetRaw("title", "hello")
	field := collection.Fields.GetByName("label").(*core.ComputedField)

	value, err := core.EvaluateComputedField(context.Background(), app, record, field)
	if err != nil {
		t.Fatalf("expected expression to succeed: %v", err)
	}
	if value != "HELLO!" {
		t.Fatalf("expected computed label, got %#v", value)
	}
	if _, exists := record.DBExport(app)["label"]; exists {
		t.Fatal("computed field must not be persisted")
	}
}

func TestComputedFieldFunctionFailureFailsBatch(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	collection := core.NewBaseCollection("computed_test")
	collection.Fields.Add(
		&core.TextField{Name: "title"},
		&core.ComputedField{
			Name:     "label",
			Mode:     core.ComputedModeFunction,
			Function: `if (record.title === 'boom') { throw new Error('boom'); } return record.title;`,
			Deps:     []string{"title"},
		},
	)

	okRecord := core.NewRecord(collection)
	okRecord.SetRaw("title", "ok")
	badRecord := core.NewRecord(collection)
	badRecord.SetRaw("title", "boom")

	err := core.PrepareVisibleComputedFields(app, nil, []*core.Record{okRecord, badRecord})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected batch error, got %v", err)
	}
	if got := okRecord.GetRaw("label"); got != nil {
		t.Fatalf("successful rows must not expose partial computed values, got %#v", got)
	}
}

func TestComputedFieldTimeout(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	collection := core.NewBaseCollection("computed_test")
	collection.Fields.Add(&core.ComputedField{
		Name:       "loop",
		Expression: `while (true) {}`,
		TimeoutMS:  5,
	})
	record := core.NewRecord(collection)
	field := collection.Fields.GetByName("loop").(*core.ComputedField)

	start := time.Now()
	_, err := core.EvaluateComputedField(context.Background(), app, record, field)
	elapsed := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("expected timeout error, got %v", err)
	}
	if elapsed > time.Second {
		t.Fatalf("script was not interrupted promptly: %s", elapsed)
	}
}

func TestComputedFieldBlocksDangerousGlobals(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	collection := core.NewBaseCollection("computed_test")
	collection.Fields.Add(&core.ComputedField{
		Name:       "dangerous",
		Expression: `typeof Function === 'undefined' && typeof process === 'undefined' && typeof require === 'undefined'`,
	})
	record := core.NewRecord(collection)
	field := collection.Fields.GetByName("dangerous").(*core.ComputedField)

	value, err := core.EvaluateComputedField(context.Background(), app, record, field)
	if err != nil {
		t.Fatal(err)
	}
	if value != true {
		t.Fatalf("expected dangerous globals to be unavailable, got %#v", value)
	}
}

func TestComputedFieldValidationRejectsRecursionAndHiddenDeps(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	collection := core.NewBaseCollection("computed_test")
	collection.Fields.Add(
		&core.TextField{Name: "secret", Hidden: true},
		&core.ComputedField{Name: "public_label", Expression: `record.secret`, Deps: []string{"secret"}},
	)
	publicComputed := collection.Fields.GetByName("public_label")
	if err := publicComputed.ValidateSettings(context.Background(), app, collection); err == nil {
		t.Fatal("expected visible computed field depending on hidden field to be rejected")
	}

	collection.Fields.RemoveByName("public_label")
	collection.Fields.Add(&core.ComputedField{
		Name:       "self",
		Expression: `record.self`,
		Deps:       []string{"self"},
	})
	selfComputed := collection.Fields.GetByName("self")
	if err := selfComputed.ValidateSettings(context.Background(), app, collection); err == nil {
		t.Fatal("expected self dependency to be rejected")
	}
}

func TestComputedFieldVisibilityAcrossAuthViews(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	collection := core.NewBaseCollection("computed_visibility")
	collection.Fields.Add(
		&core.TextField{Name: "title"},
		&core.TextField{Name: "internal_note", Hidden: true},
		&core.ComputedField{
			Name:       "public_label",
			Expression: `record.title`,
			Deps:       []string{"title"},
		},
		&core.ComputedField{
			Name:       "secret_label",
			Hidden:     true,
			Expression: `record.internal_note`,
			Deps:       []string{"internal_note"},
		},
	)
	if err := collection.ValidateSettings(context.Background(), app, collection); err != nil {
		t.Fatalf("expected schema with hidden computed depending on hidden field to be valid: %v", err)
	}

	record := core.NewRecord(collection)
	record.SetRaw("title", "public")
	record.SetRaw("internal_note", "secret")
	if err := core.PrepareVisibleComputedFields(app, nil, []*core.Record{record}); err != nil {
		t.Fatal(err)
	}

	guestExport := record.PublicExport()
	if _, ok := guestExport["public_label"]; !ok {
		t.Error("guest should see public_label")
	}
	if _, ok := guestExport["secret_label"]; ok {
		t.Error("guest must not see hidden secret_label")
	}
	if _, ok := guestExport["internal_note"]; ok {
		t.Error("guest must not see hidden dependency internal_note")
	}

	superuser, err := app.FindFirstRecordByData(core.CollectionNameSuperusers, "email", "test@example.com")
	if err != nil {
		t.Fatal(err)
	}
	record.Unhide(collection.Fields.FieldNames()...)
	if err := core.PrepareVisibleComputedFields(app, &core.RequestInfo{Auth: superuser}, []*core.Record{record}); err != nil {
		t.Fatalf("superuser computed evaluation failed: %v", err)
	}
	superExport := record.PublicExport()
	if _, ok := superExport["secret_label"]; !ok {
		t.Error("unhidden superuser view should expose secret_label")
	}
	if got := superExport["secret_label"]; got != "secret" {
		t.Fatalf("expected secret_label to use hidden dependency value, got %#v", got)
	}
}

func TestComputedFieldUndeclaredRelationRejectedAtRuntime(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	customers := core.NewBaseCollection("computed_customers")
	customers.Fields.Add(&core.TextField{Name: "name"})
	if err := app.Save(customers); err != nil {
		t.Fatal(err)
	}
	defer app.Delete(customers)

	orders := core.NewBaseCollection("computed_orders")
	orders.Fields.Add(
		&core.RelationField{Name: "customer", CollectionId: customers.Id},
		&core.ComputedField{
			Name:       "label",
			Expression: `relation('customer').name`,
			// Relations intentionally omitted.
		},
	)
	if err := app.Save(orders); err != nil {
		t.Fatal(err)
	}
	defer app.Delete(orders)

	customer := core.NewRecord(customers)
	customer.SetRaw("name", "Acme")
	if err := app.Save(customer); err != nil {
		t.Fatal(err)
	}
	order := core.NewRecord(orders)
	order.Set("customer", customer.Id)
	if err := app.Save(order); err != nil {
		t.Fatal(err)
	}

	err := core.PrepareVisibleComputedFields(app, nil, []*core.Record{order})
	if err == nil || !strings.Contains(err.Error(), "not declared") {
		t.Fatalf("expected undeclared relation error, got %v", err)
	}
}

func TestComputedFieldSchemaIsVirtual(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	collection := core.NewBaseCollection("computed_virtual")
	collection.Fields.Add(&core.ComputedField{Name: "label", Expression: `'x'`})
	if err := app.Save(collection); err != nil {
		t.Fatal(err)
	}
	defer app.Delete(collection)
	if !app.HasTable(collection.Name) {
		t.Fatal("expected collection table to exist")
	}

	type tableColumn struct {
		Name string `db:"name"`
	}
	cols := []tableColumn{}
	if err := app.NonconcurrentDB().NewQuery("PRAGMA table_info(" + collection.Name + ")").All(&cols); err != nil {
		t.Fatal(err)
	}
	for _, col := range cols {
		if col.Name == "label" {
			t.Fatal("computed field must not create a database column")
		}
	}
}
