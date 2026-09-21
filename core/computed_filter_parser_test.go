package core

import "testing"

func TestCFParserSimple(t *testing.T) {
	cases := []struct {
		in string
		ok bool
	}{
		{"label = 'a x3'", true},
		{"qty > 2 && upper != 'X'", true},
		{"(a = 1 || b = 2) && c ~ 'x'", true},
		{"flag = true", true},
		{"x = null", true},
		{"n = -3.5", true},
		{"tags:each = 'a'", true},
		{"data['k'] = 'v'", true},
		{"@request.auth.id = 'u1'", true},
		{"rel.name = 'bob'", true},
		{"a ==", false},
		{"(a = 1", false},
	}

	for _, c := range cases {
		_, err := ParseComputedFilter(c.in)
		if c.ok && err != nil {
			t.Errorf("expected %q to parse, got %v", c.in, err)
		}
		if !c.ok && err == nil {
			t.Errorf("expected %q to fail parsing", c.in)
		}
	}
}

func TestCFSQLSafeRewrite(t *testing.T) {
	c := NewBaseCollection("t")
	c.Fields.Add(
		&TextField{Name: "title"},
		&NumberField{Name: "qty"},
		&ComputedField{Name: "label", Mode: ComputedModeExpression, Expression: "doc.title", DependsOn: []string{"title"}, ResultType: "text"},
	)

	f, err := ParseComputedFilter("label = 'x' && qty > 2")
	if err != nil {
		t.Fatal(err)
	}
	sql := f.sqlSafeFilter(c)
	if sql != "id != '' && qty > 2" {
		t.Fatalf("unexpected SQL rewrite: %s", sql)
	}

	if !f.ReferencesComputedFields(c) {
		t.Fatal("expected computed ref")
	}

	f2, _ := ParseComputedFilter("title = 'a' || qty = 1")
	if f2.ReferencesComputedFields(c) {
		t.Fatal("did not expect computed refs")
	}
}
