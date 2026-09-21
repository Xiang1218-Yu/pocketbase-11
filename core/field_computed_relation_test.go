package core_test

import (
	"testing"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
)

// TestComputedRelationAccess verifies that a function-mode computed field can
// traverse a declared relation dependency using doc.$rel.
func TestComputedRelationAccess(t *testing.T) {
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()

	customers := core.NewBaseCollection("cc_customers")
	customers.Fields.Add(&core.TextField{Name: "name"})
	if err := app.Save(customers); err != nil {
		t.Fatal(err)
	}
	defer app.Delete(customers)

	orders := core.NewBaseCollection("cc_orders")
	orders.Fields.Add(
		&core.TextField{Name: "title"},
		&core.RelationField{Name: "customer", CollectionId: customers.Id, MaxSelect: 1},
		&core.ComputedField{
			Name:       "display",
			Mode:       core.ComputedModeFunction,
			Expression: "var c = doc.$rel('customer'); return c ? (doc.title + ' -> ' + c.name) : doc.title",
			DependsOn:  []string{"title", "customer"},
			ResultType: "text",
		},
	)
	if err := app.Save(orders); err != nil {
		t.Fatal(err)
	}
	defer app.Delete(orders)

	customer := core.NewRecord(customers)
	customer.Set("name", "Acme")
	if err := app.Save(customer); err != nil {
		t.Fatal(err)
	}

	order := core.NewRecord(orders)
	order.Set("title", "ord-1")
	order.Set("customer", customer.Id)
	if err := app.Save(order); err != nil {
		t.Fatal(err)
	}

	if err := app.ComputedResolveOne(order); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if got := order.GetString("display"); got != "ord-1 -> Acme" {
		t.Fatalf("expected display %q, got %q", "ord-1 -> Acme", got)
	}
}

// TestComputedUndeclaredRelationForbidden verifies that $rel can only access
// relations listed in dependsOn.
func TestComputedUndeclaredRelationForbidden(t *testing.T) {
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()

	related := core.NewBaseCollection("cc_related")
	related.Fields.Add(&core.TextField{Name: "name"})
	if err := app.Save(related); err != nil {
		t.Fatal(err)
	}
	defer app.Delete(related)

	main := core.NewBaseCollection("cc_main_undeclared")
	main.Fields.Add(
		&core.RelationField{Name: "rel", CollectionId: related.Id, MaxSelect: 1},
		&core.ComputedField{
			Name:       "v",
			Mode:       core.ComputedModeFunction,
			Expression: "var r = doc.$rel('rel'); return r ? r.name : ''",
			DependsOn:  nil,
			ResultType: "text",
		},
	)
	if err := app.Save(main); err != nil {
		t.Fatal(err)
	}
	defer app.Delete(main)

	record := core.NewRecord(main)
	if err := app.ComputedResolveOne(record); err == nil {
		t.Fatal("expected forbidden undeclared relation access error")
	}
}

// TestComputedCacheInvalidationOnSchemaChange makes sure changing a computed
// expression takes effect immediately (content-addressed compile cache).
func TestComputedCacheInvalidationOnSchemaChange(t *testing.T) {
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()

	c := core.NewBaseCollection("cc_cache_inv")
	c.Fields.Add(
		&core.TextField{Name: "title"},
		&core.ComputedField{
			Name:       "v",
			Mode:       core.ComputedModeExpression,
			Expression: "doc.title",
			DependsOn:  []string{"title"},
			ResultType: "text",
		},
	)
	if err := app.Save(c); err != nil {
		t.Fatal(err)
	}
	defer app.Delete(c)

	r1 := core.NewRecord(c)
	r1.Set("title", "one")
	if err := app.ComputedResolveOne(r1); err != nil {
		t.Fatal(err)
	}
	if r1.GetString("v") != "one" {
		t.Fatalf("expected one, got %q", r1.GetString("v"))
	}

	// fetch the persisted collection, change the expression and save
	c2, err := app.FindCollectionByNameOrId(c.Id)
	if err != nil {
		t.Fatal(err)
	}
	cf := c2.Fields.GetByName("v").(*core.ComputedField)
	cf.Expression = "doc.title + '!'"
	if err := app.Save(c2); err != nil {
		t.Fatal(err)
	}

	c3, err := app.FindCachedCollectionByNameOrId(c.Id)
	if err != nil {
		t.Fatal(err)
	}
	r2 := core.NewRecord(c3)
	r2.Set("title", "two")
	if err := app.ComputedResolveOne(r2); err != nil {
		t.Fatal(err)
	}
	if r2.GetString("v") != "two!" {
		t.Fatalf("expected updated expression result two!, got %q", r2.GetString("v"))
	}
}
