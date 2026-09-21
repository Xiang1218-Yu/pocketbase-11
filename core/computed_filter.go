package core

import (
	"fmt"
	"strings"

	"github.com/spf13/cast"
)

// ComputedFilter is a parsed in-memory filter expression that may reference
// computed fields.
type ComputedFilter struct {
	raw  string
	root cfNode
}

// ParseComputedFilter parses the provided fexpr-like filter string.
func ParseComputedFilter(raw string) (*ComputedFilter, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return &ComputedFilter{raw: raw}, nil
	}

	parser := newCFParser(raw)
	root, err := parser.parse()
	if err != nil {
		return nil, err
	}

	return &ComputedFilter{raw: raw, root: root}, nil
}

// IsEmpty reports whether the filter is blank.
func (f *ComputedFilter) IsEmpty() bool {
	return f == nil || f.root == nil
}

// referencedIdentifiers collects all identifier paths used in the expression.
func (f *ComputedFilter) referencedIdentifiers() []string {
	if f == nil || f.root == nil {
		return nil
	}
	out := map[string]bool{}
	var walk func(n cfNode)
	walk = func(n cfNode) {
		switch x := n.(type) {
		case *cfBinary:
			walk(x.left)
			walk(x.right)
		case *cfComparison:
			out[x.identifier] = true
		}
	}
	walk(f.root)

	keys := make([]string, 0, len(out))
	for k := range out {
		keys = append(keys, k)
	}
	return keys
}

// rootFieldName returns the base (first path segment, modifiers stripped)
// identifier of a comparison path.
func rootFieldName(ident string) string {
	// strip modifier suffix like ":each"
	if idx := strings.Index(ident, ":"); idx >= 0 {
		ident = ident[:idx]
	}
	// strip relational path
	if idx := strings.Index(ident, "."); idx >= 0 {
		ident = ident[:idx]
	}
	return ident
}

// DottedRelationExpands returns the dot-separated paths referenced in the
// expression whose root field is a relation field of the base collection
// (eg. "customer.address.city").
//
// Only the root segment needs to be verified against the base collection;
// the remaining path validity is enforced by the SQL pre-filter and the expand
// engine. Paths into plain/JSON fields are excluded.
func (f *ComputedFilter) DottedRelationExpands(collection *Collection) []string {
	if f == nil || f.root == nil {
		return nil
	}

	out := map[string]bool{}
	for _, ident := range f.referencedIdentifiers() {
		if strings.HasPrefix(ident, "@") || !strings.Contains(ident, ".") {
			continue
		}
		// strip modifier
		path := ident
		if i := strings.Index(path, ":"); i >= 0 {
			path = path[:i]
		}
		root := strings.SplitN(path, ".", 2)[0]
		if _, ok := collection.Fields.GetByName(root).(*RelationField); !ok {
			continue
		}

		// only relation segments must be expanded - the terminal segment is a
		// plain field on the related record and is read directly during eval.
		// We conservatively emit the full path minus the last segment (when
		// there are at least 2 segments), otherwise the root relation.
		segments := strings.Split(path, ".")
		expandPath := strings.Join(segments[:len(segments)-1], ".")
		if expandPath == "" {
			continue
		}
		out[expandPath] = true
	}

	result := make([]string, 0, len(out))
	for p := range out {
		result = append(result, p)
	}
	return result
}

// ReferencesComputedFields reports whether any comparison references a
// computed field of the collection.
func (f *ComputedFilter) ReferencesComputedFields(collection *Collection) bool {
	if f == nil {
		return false
	}
	computed := map[string]bool{}
	for _, cf := range ComputedFieldsOf(collection) {
		computed[cf.Name] = true
	}
	for _, ident := range f.referencedIdentifiers() {
		root := rootFieldName(ident)
		if computed[root] {
			return true
		}
	}
	return false
}

// ReferencedComputedFields returns the unique computed field names used in the filter.
func (f *ComputedFilter) ReferencedComputedFields(collection *Collection) []string {
	return f.referencedComputedFields(collection)
}

// referencedComputedFields returns the unique computed field names used.
func (f *ComputedFilter) referencedComputedFields(collection *Collection) []string {
	computed := map[string]bool{}
	for _, cf := range ComputedFieldsOf(collection) {
		computed[cf.Name] = true
	}
	seen := map[string]bool{}
	out := []string{}
	for _, ident := range f.referencedIdentifiers() {
		root := rootFieldName(ident)
		if computed[root] && !seen[root] {
			seen[root] = true
			out = append(out, root)
		}
	}
	return out
}

// SQLSafeFilterForTest exposes sqlSafeFilter for tests.
func (f *ComputedFilter) SQLSafeFilterForTest(collection *Collection) string {
	return f.sqlSafeFilter(collection)
}

// sqlSafeFilter returns a filter string in which every comparison referencing
// a computed field is replaced with TRUE, leaving only SQL-resolvable
// predicates (regular columns, @request, relations).
//
// The result is used as the SQL pre-filter; the full original expression is
// still evaluated in-memory on the hydrated records, so semantics are
// preserved (TRUE is neutral under AND; in-memory filtering removes false
// positives introduced by the OR cases).
func (f *ComputedFilter) sqlSafeFilter(collection *Collection) string {
	if f == nil || f.root == nil {
		return f.raw
	}
	computed := map[string]bool{}
	for _, cf := range ComputedFieldsOf(collection) {
		computed[cf.Name] = true
	}
	return rewriteComputedComparisonsAsTrue(f.raw, f, computed)
}

// computedSQLTautology is a SQL expression that always evaluates to true for
// records with an id (all record rows). It is used when stripping out
// computed-only comparisons from the SQL pre-filter.
const computedSQLTautology = "id != ''"

// rewriteComputedComparisonsAsTrue reconstructs the filter with computed
// comparisons replaced by a SQL tautology.
//
// It uses the parsed AST and re-emits it; non-computed comparisons are
// re-serialized from the parsed comparison nodes.
func rewriteComputedComparisonsAsTrue(raw string, f *ComputedFilter, computed map[string]bool) string {
	var emit func(n cfNode) string

	emit = func(n cfNode) string {
		switch x := n.(type) {
		case *cfBinary:
			op := "&&"
			if x.op == "||" {
				op = "||"
			}
			return emit(x.left) + " " + op + " " + emit(x.right)
		case *cfComparison:
			root := rootFieldName(x.identifier)
			if computed[root] || (strings.Contains(x.identifier, ".") && computed[root]) {
				return computedSQLTautology
			}
			return emitComparisonRaw(x)
		}
		return computedSQLTautology
	}

	return emit(f.root)
}

func emitComparisonRaw(c *cfComparison) string {
	var sb strings.Builder
	sb.WriteString(c.identifier)
	if c.indexKey != "" {
		sb.WriteString("[")
		sb.WriteString(c.indexKey)
		sb.WriteString("]")
	}
	sb.WriteString(" ")
	sb.WriteString(c.op)
	sb.WriteString(" ")
	switch c.value.kind {
	case cfValString:
		sb.WriteString(quoteCFString(c.value.str))
	case cfValNumber:
		sb.WriteString(strings.TrimRight(strings.TrimRight(fmt.Sprintf("%f", c.value.num), "0"), "."))
		if strings.HasSuffix(sb.String(), ".") {
			sb.WriteString("0")
		}
	case cfValBool:
		if c.value.boolv {
			sb.WriteString("true")
		} else {
			sb.WriteString("false")
		}
	case cfValNull:
		sb.WriteString("null")
	}
	return sb.String()
}

func quoteCFString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "'", `\'`)
	return "'" + s + "'"
}

// Eval evaluates the filter against a fully hydrated record.
func (f *ComputedFilter) Eval(record *Record, req *ComputeRequest) bool {
	if f == nil || f.root == nil {
		return true
	}
	ctx := &cfEvalCtx{record: record, req: req}
	return ctx.eval(f.root)
}

type cfEvalCtx struct {
	record *Record
	req    *ComputeRequest
}

func (c *cfEvalCtx) eval(n cfNode) bool {
	switch x := n.(type) {
	case *cfBinary:
		if x.op == "&&" {
			return c.eval(x.left) && c.eval(x.right)
		}
		return c.eval(x.left) || c.eval(x.right)
	case *cfComparison:
		return c.evalComparison(x)
	}
	return true
}

// evalComparison resolves a value from the record and applies the operator.
func (c *cfEvalCtx) evalComparison(x *cfComparison) (result bool) {
	left := c.resolveIdentifier(x.identifier, x.indexKey)

	// modifiers
	path := x.identifier
	modifier := ""
	if idx := strings.Index(path, ":"); idx >= 0 {
		modifier = path[idx+1:]
		path = path[:idx]
	}

	if modifier == "isset" {
		// isset is encoded in fexpr as "field:isset = true"
		return left != nil
	}

	switch x.op {
	case "=":
		return looseEqualsAny(left, x.value)
	case "!=":
		return !looseEqualsAny(left, x.value)
	case ">", ">=", "<", "<=":
		return compareOrdered(left, x.value, x.op)
	case "~":
		return looseContains(left, x.value)
	case "!~":
		return !looseContains(left, x.value)
	}
	return false
}

// resolveIdentifier resolves a dotted/modifier path to a plain Go value from:
//   - the record (including computed field values)
//   - expanded records (record.Expand())
//   - the request auth snapshot (@request.auth.*)
func (c *cfEvalCtx) resolveIdentifier(path, indexKey string) any {
	// @request.auth.* and @request.* static values
	if strings.HasPrefix(path, "@request.") {
		if c.req == nil || c.req.config.RequestInfo == nil {
			return nil
		}
		info := c.req.config.RequestInfo
		rest := strings.TrimPrefix(path, "@request.")

		var current any

		switch {
		case rest == "auth":
			if info.Auth != nil {
				return info.Auth.PublicExport()
			}
			return nil
		case rest == "method":
			return info.Method
		case rest == "context":
			return info.Context
		case strings.HasPrefix(rest, "query."):
			current = map[string]string(info.Query)
			rest = strings.TrimPrefix(rest, "query.")
		case strings.HasPrefix(rest, "headers."):
			current = map[string]string(info.Headers)
			rest = strings.TrimPrefix(rest, "headers.")
		case strings.HasPrefix(rest, "body."):
			current = info.Body
			rest = strings.TrimPrefix(rest, "body.")
		case strings.HasPrefix(rest, "auth."):
			if info.Auth == nil {
				return nil
			}
			current = info.Auth.PublicExport()
			rest = strings.TrimPrefix(rest, "auth.")
		default:
			return nil
		}

		return walkNestedValue(current, rest, indexKey)
	}

	// regular record fields, with expand support for relation dots
	current := any(c.record)
	parts := strings.Split(path, ".")

	for i, part := range parts {
		switch rec := current.(type) {
		case *Record:
			// first segment: check expand
			if i == 0 {
				expanded, ok := rec.Expand()[part]
				if ok {
					if i == len(parts)-1 {
						return rec.Get(part)
					}
					switch ev := expanded.(type) {
					case *Record:
						current = ev
						continue
					case []*Record:
						if len(ev) > 0 {
							current = ev
							continue
						}
						return nil
					}
				}
			}
			raw := rec.Get(part)
			if i == len(parts)-1 {
				current = raw
			} else {
				// try to navigate into maps / nested records via raw value
				current = raw
			}
		case []*Record:
			// collect values from a relation list
			values := make([]any, 0, len(rec))
			for _, r := range rec {
				values = append(values, r.Get(part))
			}
			current = values
		default:
			current = walkNestedValue(current, part, "")
		}
	}

	if indexKey != "" {
		current = walkNestedValue(current, indexKey, "")
	}

	return current
}

// walkNestedValue walks into maps/slices using a dotted path.
func walkNestedValue(v any, path, indexKey string) any {
	if path == "" && indexKey == "" {
		return v
	}

	parts := []string{}
	if path != "" {
		parts = append(parts, strings.Split(path, ".")...)
	}
	if indexKey != "" {
		parts = append(parts, indexKey)
	}

	current := v
	for _, p := range parts {
		switch x := current.(type) {
		case map[string]any:
			current = x[p]
		case []any:
			idx, err := cast.ToIntE(p)
			if err != nil || idx < 0 || idx >= len(x) {
				return nil
			}
			current = x[idx]
		case []string:
			// comparing against array values directly
			return x
		default:
			return nil
		}
	}
	return current
}

// looseEqualsAny handles =/!= against scalars and arrays (any-match).
func looseEqualsAny(left any, right cfValue) bool {
	if right.kind == cfValNull {
		return left == nil
	}

	switch l := left.(type) {
	case nil:
		return false
	case []any:
		for _, item := range l {
			if looseEquals(item, right) {
				return true
			}
		}
		return false
	case []string:
		for _, item := range l {
			if looseEquals(item, right) {
				return true
			}
		}
		return false
	default:
		return looseEquals(left, right)
	}
}

func looseEquals(left any, right cfValue) bool {
	if right.kind == cfValNull {
		return left == nil
	}

	switch right.kind {
	case cfValString:
		return cast.ToString(left) == right.str
	case cfValNumber:
		n, err := cast.ToFloat64E(left)
		if err != nil {
			return false
		}
		return n == right.num
	case cfValBool:
		b, err := cast.ToBoolE(left)
		if err != nil {
			return false
		}
		return b == right.boolv
	}
	return false
}

func looseContains(left any, right cfValue) bool {
	if right.kind != cfValString {
		return false
	}
	needle := strings.ToLower(right.str)

	switch l := left.(type) {
	case nil:
		return false
	case []any:
		for _, item := range l {
			if strings.Contains(strings.ToLower(cast.ToString(item)), needle) {
				return true
			}
		}
		return false
	case []string:
		for _, item := range l {
			if strings.Contains(strings.ToLower(item), needle) {
				return true
			}
		}
		return false
	default:
		return strings.Contains(strings.ToLower(cast.ToString(left)), needle)
	}
}

func compareOrdered(left any, right cfValue, op string) bool {
	if right.kind != cfValNumber && right.kind != cfValString {
		return false
	}

	cmp := func(a string, b string) int { return strings.Compare(a, b) }
	num := func(a any) (float64, bool) {
		n, err := cast.ToFloat64E(a)
		return n, err == nil
	}

	doCompare := func(a, b float64, as, bs string) bool {
		if right.kind == cfValNumber {
			switch op {
			case ">":
				return a > b
			case ">=":
				return a >= b
			case "<":
				return a < b
			case "<=":
				return a <= b
			}
			return false
		}
		r := cmp(as, bs)
		switch op {
		case ">":
			return r > 0
		case ">=":
			return r >= 0
		case "<":
			return r < 0
		case "<=":
			return r <= 0
		}
		return false
	}

	// arrays: compare against any element
	switch l := left.(type) {
	case []any:
		for _, item := range l {
			if doSingleOrdered(item, right, num, doCompare) {
				return true
			}
		}
		return false
	case []string:
		for _, item := range l {
			if doSingleOrdered(item, right, num, doCompare) {
				return true
			}
		}
		return false
	default:
		return doSingleOrdered(left, right, num, doCompare)
	}
}

func doSingleOrdered(
	left any,
	right cfValue,
	toNum func(any) (float64, bool),
	doCompare func(a, b float64, as, bs string) bool,
) bool {
	if right.kind == cfValNumber {
		a, ok := toNum(left)
		if !ok {
			return false
		}
		return doCompare(a, right.num, "", "")
	}
	return doCompare(0, 0, cast.ToString(left), right.str)
}
