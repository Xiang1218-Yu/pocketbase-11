package apis_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
	"github.com/pocketbase/pocketbase/tools/types"
)

// setupComputedCollection creates a temporary test collection with computed
// fields used by the API scenarios below.
func setupComputedCollection(t testing.TB, app core.App, includeBroken bool) *core.Collection {
	t.Helper()

	c := core.NewBaseCollection("api_computed")
	c.ListRule = types.Pointer("")
	c.ViewRule = types.Pointer("")
	c.CreateRule = types.Pointer("")
	c.UpdateRule = types.Pointer("")

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
			Name:       "leak",
			Mode:       core.ComputedModeExpression,
			Expression: "doc.secret",
			DependsOn:  []string{"secret"},
			ResultType: "text",
		},
	)

	if includeBroken {
		c.Fields.Add(&core.ComputedField{
			Name:       "broken",
			Mode:       core.ComputedModeFunction,
			Expression: "throw new Error('boom')",
			ResultType: "text",
		})
	}

	if err := app.Save(c); err != nil {
		t.Fatal(err)
	}

	for _, vals := range [][2]any{{"apple", 2}, {"banana", 5}} {
		r := core.NewRecord(c)
		r.Set("title", vals[0])
		r.Set("qty", vals[1])
		r.Set("secret", "hidden-value")
		if err := app.Save(r); err != nil {
			t.Fatal(err)
		}
	}

	return c
}

func TestComputedRecordsList(t *testing.T) {
	t.Parallel()

	scenarios := []tests.ApiScenario{
		{
			Name:   "list includes evaluated computed fields and hides their hidden deps",
			Method: http.MethodGet,
			URL:    "/api/collections/api_computed/records?perPage=10&sort=title",
			BeforeTestFunc: func(t testing.TB, app *tests.TestApp, e *core.ServeEvent) {
				setupComputedCollection(t, app.BaseApp, false)
			},
			ExpectedStatus: 200,
			ExpectedContent: []string{
				`"label":"apple x2"`,
				`"label":"banana x5"`,
			},
			NotExpectedContent: []string{
				`"secret"`,
				`"leak"`,
				`hidden-value`,
			},
		},
		{
			Name:   "filter on computed field",
			Method: http.MethodGet,
			URL:    "/api/collections/api_computed/records?perPage=10&filter=label='apple%20x2'",
			BeforeTestFunc: func(t testing.TB, app *tests.TestApp, e *core.ServeEvent) {
				setupComputedCollection(t, app.BaseApp, false)
			},
			ExpectedStatus: 200,
			ExpectedContent: []string{
				`"label":"apple x2"`,
			},
			NotExpectedContent: []string{
				`banana`,
			},
		},
		{
			Name:   "computed evaluation error fails the whole batch (no partial data)",
			Method: http.MethodGet,
			URL:    "/api/collections/api_computed/records?perPage=10",
			BeforeTestFunc: func(t testing.TB, app *tests.TestApp, e *core.ServeEvent) {
				setupComputedCollection(t, app.BaseApp, true)
			},
			ExpectedStatus: 500,
			ExpectedContent: []string{
				"Failed to evaluate computed field(s)",
			},
			NotExpectedContent: []string{
				`"label":"apple x2"`,
				`"label":"banana x5"`,
			},
		},
		{
			Name:   "computed field is read-only on create",
			Method: http.MethodPost,
			URL:    "/api/collections/api_computed/records",
			Body:   strings.NewReader(`{"title":"pear","qty":1,"label":"hacked"}`),
			BeforeTestFunc: func(t testing.TB, app *tests.TestApp, e *core.ServeEvent) {
				setupComputedCollection(t, app.BaseApp, false)
			},
			ExpectedStatus: 200,
			ExpectedContent: []string{
				`"label":"pear x1"`,
			},
			NotExpectedContent: []string{
				`"label":"hacked"`,
			},
		},
	}

	for _, scenario := range scenarios {
		scenario := scenario
		scenario.Test(t)
	}
}
