package core_test

import (
	"sync"
	"testing"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
)

// TestComputedConcurrentResolve stress-tests the VM pool with concurrent
// evaluations, ensuring data-race-free behavior and consistent results.
func TestComputedConcurrentResolve(t *testing.T) {
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()

	c := core.NewBaseCollection("computed_concurrent")
	c.Fields.Add(
		&core.TextField{Name: "title"},
		&core.ComputedField{
			Name:       "label",
			Mode:       core.ComputedModeExpression,
			Expression: "doc.title + '!'",
			DependsOn:  []string{"title"},
			ResultType: "text",
		},
	)
	if err := app.Save(c); err != nil {
		t.Fatal(err)
	}
	defer app.Delete(c)

	const goroutines = 30
	const iterations = 20

	var wg sync.WaitGroup
	errCh := make(chan error, goroutines)

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				r := core.NewRecord(c)
				r.Set("title", "g")
				if err := app.ComputedResolveOne(r); err != nil {
					errCh <- err
					return
				}
				if got := r.GetString("label"); got != "g!" {
					t.Errorf("unexpected label %q", got)
					return
				}
			}
		}(i)
	}

	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
}
