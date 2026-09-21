package apis_test

import (
	"net/http"
	"testing"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
	"github.com/pocketbase/pocketbase/tools/types"
)

// TestComputedFieldHiddenDependencyByAuth verifies that computed fields whose
// dependencies are hidden are consistently hidden across guest and regular
// user auth perspectives, while superusers see everything.
func TestComputedFieldHiddenDependencyByAuth(t *testing.T) {
	t.Parallel()

	// regular user token from the test fixture (users collection)
	const userToken = "eyJhbGciOiJIUzI1NiJ9.eyJpZCI6IjRxMXhsY2xtZmxva3UzMyIsInR5cGUiOiJhdXRoIiwiY29sbGVjdGlvbklkIjoiX3BiX3VzZXJzX2F1dGhfIiwiZXhwIjoyNTI0NjA0NDYxLCJyZWZyZXNoYWJsZSI6dHJ1ZX0.ZT3F0Z3iM-xbGgSG3LEKiEzHrPHr8t8IuHLZGGNuxLo"

	// mint a superuser token from the fixture data (the auth secret is part
	// of the copied test database so the resulting token is valid for each
	// scenario's test app)
	tokenApp, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	su, err := tokenApp.FindFirstRecordByData(core.CollectionNameSuperusers, "email", "test@example.com")
	if err != nil {
		t.Fatal(err)
	}
	superuserToken, err := su.NewAuthToken()
	if err != nil {
		t.Fatal(err)
	}
	tokenApp.Cleanup()

	setup := func(t testing.TB, app core.App) {
		c := core.NewBaseCollection("api_computed_auth")
		c.ListRule = types.Pointer("")
		c.ViewRule = types.Pointer("")
		c.Fields.Add(
			&core.TextField{Name: "title"},
			&core.TextField{Name: "secret", Hidden: true},
			&core.ComputedField{
				Name:       "leak",
				Mode:       core.ComputedModeExpression,
				Expression: "doc.secret",
				DependsOn:  []string{"secret"},
				ResultType: "text",
			},
			&core.ComputedField{
				Name:       "safe",
				Mode:       core.ComputedModeExpression,
				Expression: "doc.title + '!'",
				DependsOn:  []string{"title"},
				ResultType: "text",
			},
		)
		if err := app.Save(c); err != nil {
			t.Fatal(err)
		}
		r := core.NewRecord(c)
		r.Set("title", "public")
		r.Set("secret", "topsecret")
		if err := app.Save(r); err != nil {
			t.Fatal(err)
		}
	}

	mkScenario := func(name, authToken string, expectLeak bool) tests.ApiScenario {
		headers := map[string]string{}
		if authToken != "" {
			headers["Authorization"] = authToken
		}

		s := tests.ApiScenario{
			Name:           name,
			Method:         http.MethodGet,
			URL:            "/api/collections/api_computed_auth/records",
			Headers:        headers,
			ExpectedStatus: 200,
			BeforeTestFunc: func(t testing.TB, app *tests.TestApp, e *core.ServeEvent) {
				setup(t, app.BaseApp)
			},
			ExpectedContent: []string{
				`"safe":"public!"`,
			},
		}

		if expectLeak {
			s.ExpectedContent = append(s.ExpectedContent, `"leak":"topsecret"`, `"secret":"topsecret"`)
		} else {
			s.NotExpectedContent = []string{`"leak"`, "topsecret", `"secret"`}
		}

		return s
	}

	g := mkScenario("guest: hidden dependency hides the computed field", "", false)
	g.Test(t)
	u := mkScenario("regular user: hidden dependency still hides the computed field", userToken, false)
	u.Test(t)
	s := mkScenario("superuser: hidden dependency is visible through computed field", superuserToken, true)
	s.Test(t)
}
