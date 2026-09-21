package core_test

import (
	"testing"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
)

// TestComputedSchemaMigrationAndRollback verifies that adding and removing a
// computed field through the standard collection schema mechanisms:
//   - does not create/drop a physical SQL column,
//   - leaves existing records intact,
//   - and removing the field (rollback) works without data migration errors.
func TestComputedSchemaMigrationAndRollback(t *testing.T) {
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()

	c := core.NewBaseCollection("computed_migration")
	c.Fields.Add(&core.TextField{Name: "title"})
	if err := app.Save(c); err != nil {
		t.Fatal(err)
	}

	r := core.NewRecord(c)
	r.Set("title", "hello")
	if err := app.Save(r); err != nil {
		t.Fatal(err)
	}

	// migration "up": add a computed field
	c.Fields.Add(&core.ComputedField{
		Name:       "label",
		Mode:       core.ComputedModeExpression,
		Expression: "doc.title + '!'",
		DependsOn:  []string{"title"},
		ResultType: "text",
	})
	if err := app.Save(c); err != nil {
		t.Fatalf("add computed field: %v", err)
	}

	// the record table must NOT contain a label column
	type colInfo struct {
		Name string `db:"name"`
	}
	cols := []colInfo{}
	if err := app.ConcurrentDB().NewQuery("PRAGMA table_info([[computed_migration]])").All(&cols); err != nil {
		t.Fatal(err)
	}
	for _, col := range cols {
		if col.Name == "label" {
			t.Fatal("computed field must not create a physical column")
		}
	}

	// existing record still loads and resolves
	r2, err := app.FindRecordById(c, r.Id)
	if err != nil {
		t.Fatal(err)
	}
	if r2.GetString("title") != "hello" {
		t.Fatalf("unexpected title %q", r2.GetString("title"))
	}
	if err := app.ComputedResolveOne(r2); err != nil {
		t.Fatal(err)
	}
	if r2.GetString("label") != "hello!" {
		t.Fatalf("expected computed label, got %q", r2.GetString("label"))
	}

	// migration "down": remove the computed field
	c2, err := app.FindCollectionByNameOrId(c.Id)
	if err != nil {
		t.Fatal(err)
	}
	c2.Fields.RemoveByName("label")
	if err := app.Save(c2); err != nil {
		t.Fatalf("remove computed field: %v", err)
	}

	r3, err := app.FindRecordById(c2, r.Id)
	if err != nil {
		t.Fatal(err)
	}
	if r3.GetString("title") != "hello" {
		t.Fatalf("record data lost after rollback: %q", r3.GetString("title"))
	}

	if err := app.Delete(c); err != nil {
		t.Fatal(err)
	}
}
