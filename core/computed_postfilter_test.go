package core_test

import (
	"testing"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
	"github.com/pocketbase/pocketbase/tools/types"
)

func TestComputedPostFilterCore(t *testing.T) {
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()

	customers := core.NewBaseCollection("dbg_pf_customers")
	customers.Fields.Add(&core.TextField{Name: "name"})
	customers.ListRule = types.Pointer("")
	customers.ViewRule = types.Pointer("")
	if err := app.Save(customers); err != nil {
		t.Fatal(err)
	}

	c := core.NewBaseCollection("dbg_postfilter")
	c.ListRule = types.Pointer("")
	c.Fields.Add(
		&core.TextField{Name: "title"},
		&core.NumberField{Name: "qty"},
		&core.RelationField{Name: "customer", CollectionId: customers.Id, MaxSelect: 1},
		&core.ComputedField{
			Name:       "label",
			Mode:       core.ComputedModeExpression,
			Expression: "doc.title + ' x' + doc.qty",
			DependsOn:  []string{"title", "qty"},
			ResultType: "text",
		},
	)
	if err := app.Save(c); err != nil {
		t.Fatal(err)
	}

	cust := core.NewRecord(customers)
	cust.Set("name", "Acme")
	if err := app.Save(cust); err != nil {
		t.Fatal(err)
	}

	for _, vals := range []struct {
		title    string
		qty      int
		customer string
	}{
		{"apple", 2, cust.Id},
		{"banana", 5, ""},
	} {
		r := core.NewRecord(c)
		r.Set("title", vals.title)
		r.Set("qty", vals.qty)
		if vals.customer != "" {
			r.Set("customer", vals.customer)
		}
		if err := app.Save(r); err != nil {
			t.Fatal(err)
		}
	}

	info := &core.RequestInfo{Method: "GET", Context: core.RequestInfoContextRealtime}

	t.Run("computed-only filter", func(t *testing.T) {
		f, err := core.ParseComputedFilter("label='apple x2'")
		if err != nil {
			t.Fatal(err)
		}

		res, err := core.ExecComputedPostFilter(core.ComputedListRequest{
			App:         app,
			Collection:  c,
			RequestInfo: info,
			Filter:      f,
			Page:        1,
			PerPage:     30,
			BuildBaseQuery: func() *dbx.SelectQuery {
				return app.RecordQuery(c)
			},
		})
		if err != nil {
			t.Fatalf("ExecComputedPostFilter: %v", err)
		}

		if res.TotalItems != 1 || len(res.Records) != 1 {
			t.Fatalf("expected 1 match, got %d (%v)", res.TotalItems, res.Records)
		}
		if got := res.Records[0].GetString("label"); got != "apple x2" {
			t.Fatalf("unexpected label %q", got)
		}
	})

	t.Run("mixed relation and computed filter", func(t *testing.T) {
		f, err := core.ParseComputedFilter("customer.name='Acme' && label ~ 'apple'")
		if err != nil {
			t.Fatal(err)
		}

		res, err := core.ExecComputedPostFilter(core.ComputedListRequest{
			App:         app,
			Collection:  c,
			RequestInfo: info,
			Filter:      f,
			Page:        1,
			PerPage:     30,
			BuildBaseQuery: func() *dbx.SelectQuery {
				return app.RecordQuery(c)
			},
		})
		if err != nil {
			t.Fatalf("ExecComputedPostFilter: %v", err)
		}

		if res.TotalItems != 1 || len(res.Records) != 1 {
			t.Fatalf("expected 1 match, got %d (%v)", res.TotalItems, res.Records)
		}
		if got := res.Records[0].GetString("title"); got != "apple" {
			t.Fatalf("unexpected title %q", got)
		}
	})

	t.Run("or filter excludes non-matching rows", func(t *testing.T) {
		f, err := core.ParseComputedFilter("label='banana x5' || qty = 99")
		if err != nil {
			t.Fatal(err)
		}

		res, err := core.ExecComputedPostFilter(core.ComputedListRequest{
			App:         app,
			Collection:  c,
			RequestInfo: info,
			Filter:      f,
			Page:        1,
			PerPage:     30,
			BuildBaseQuery: func() *dbx.SelectQuery {
				return app.RecordQuery(c)
			},
		})
		if err != nil {
			t.Fatalf("ExecComputedPostFilter: %v", err)
		}

		if res.TotalItems != 1 || len(res.Records) != 1 {
			t.Fatalf("expected 1 match, got %d (%v)", res.TotalItems, res.Records)
		}
	})
}
