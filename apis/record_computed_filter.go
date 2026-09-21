package apis

import (
	"fmt"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/ganigeorgiev/fexpr"
	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/search"
	"github.com/spf13/cast"
)

func filterUsesComputedField(collection *core.Collection, rawFilter string) (bool, error) {
	if strings.TrimSpace(rawFilter) == "" {
		return false, nil
	}
	groups, err := fexpr.Parse(rawFilter)
	if err != nil {
		return false, err
	}
	var walkGroup func(group fexpr.ExprGroup) bool
	var walkGroups func(groups []fexpr.ExprGroup) bool
	walkGroup = func(group fexpr.ExprGroup) bool {
		switch item := group.Item.(type) {
		case fexpr.Expr:
			return tokenUsesComputedField(collection, item.Left) || tokenUsesComputedField(collection, item.Right)
		case fexpr.ExprGroup:
			return walkGroup(item)
		case []fexpr.ExprGroup:
			return walkGroups(item)
		}
		return false
	}
	walkGroups = func(groups []fexpr.ExprGroup) bool {
		for _, group := range groups {
			if walkGroup(group) {
				return true
			}
		}
		return false
	}
	return walkGroups(groups), nil
}

func tokenUsesComputedField(collection *core.Collection, token fexpr.Token) bool {
	if token.Type != fexpr.TokenIdentifier {
		return false
	}
	path := strings.SplitN(token.Literal, ":", 2)[0]
	parts := strings.Split(path, ".")
	if len(parts) == 0 {
		return false
	}
	field := collection.Fields.GetByName(parts[0])
	return field != nil && field.Type() == core.FieldTypeComputed
}

const defaultComputedScanLimit = search.MaxPerPage

func recordsListWithComputedFilter(
	e *core.RequestEvent,
	collection *core.Collection,
	query *dbx.SelectQuery,
	requestInfo *core.RequestInfo,
	rawFilter string,
) error {
	values, err := url.ParseQuery(e.Request.URL.Query().Encode())
	if err != nil {
		return e.BadRequestError("Invalid query parameters.", err)
	}

	page := 1
	if raw := values.Get(search.PageQueryParam); raw != "" {
		page, err = strconv.Atoi(raw)
		if err != nil || page < 1 {
			page = 1
		}
	}
	perPage := search.DefaultPerPage
	if raw := values.Get(search.PerPageQueryParam); raw != "" {
		perPage, err = strconv.Atoi(raw)
		if err != nil || perPage < 1 {
			perPage = search.DefaultPerPage
		}
		if perPage > search.MaxPerPage {
			perPage = search.MaxPerPage
		}
	}
	skipTotal := values.Get(search.SkipTotalQueryParam) == "true"
	sortFields := search.ParseSortFromString(values.Get(search.SortQueryParam))
	usesComputedSort := false
	sqlSorts := []string{}
	for _, sortField := range sortFields {
		if field := collection.Fields.GetByName(sortField.Name); field != nil && field.Type() == core.FieldTypeComputed {
			usesComputedSort = true
			continue
		}
		prefix := ""
		if sortField.Direction == search.SortDesc {
			prefix = "-"
		}
		sqlSorts = append(sqlSorts, prefix+sortField.Name)
	}

	// SQL still applies access rules, non-computed expression validation happens
	// in memory together with the computed expression.
	values.Del(search.FilterQueryParam)
	values.Del(search.PageQueryParam)
	values.Del(search.PerPageQueryParam)
	values.Del(search.SkipTotalQueryParam)
	if usesComputedSort {
		if len(sqlSorts) > 0 {
			values.Set(search.SortQueryParam, strings.Join(sqlSorts, ","))
		} else {
			values.Del(search.SortQueryParam)
		}
	}

	resolver := core.NewRecordFieldResolver(e.App, collection, requestInfo, true)
	resolver.SetAllowHiddenFields(requestInfo.HasSuperuserAuth())
	// Provider caps fetched rows to MaxPerPage; computed predicates are
	// evaluated in Go because they have no SQL representation.
	provider := search.NewProvider(resolver).Query(query).SkipTotal(true).PerPage(defaultComputedScanLimit)
	if err := provider.Parse(values.Encode()); err != nil {
		return e.BadRequestError("Invalid list query.", err)
	}

	candidates := []*core.Record{}
	if _, err := provider.Exec(&candidates); err != nil {
		return e.BadRequestError("Invalid list query.", err)
	}

	if requestInfo.HasSuperuserAuth() {
		for _, record := range candidates {
			record.Unhide(collection.Fields.FieldNames()...).IgnoreEmailVisibility(true)
		}
	}
	if err := core.PrepareVisibleComputedFields(e.App, requestInfo, candidates); err != nil {
		return e.InternalServerError("Failed to evaluate computed fields.", err)
	}
	if err := expandComputedFilterRelations(e.App, requestInfo, candidates, rawFilter); err != nil {
		return e.BadRequestError("Invalid filter.", err)
	}

	records, err := evaluateInMemoryFilter(collection, candidates, rawFilter, requestInfo)
	if err != nil {
		return e.BadRequestError("Invalid filter.", err)
	}
	if usesComputedSort {
		sortRecordsInMemory(records, sortFields)
	}

	totalItems := -1
	totalPages := -1
	if !skipTotal {
		totalItems = len(records)
		totalPages = int(math.Ceil(float64(totalItems) / float64(perPage)))
	}

	start := (page - 1) * perPage
	if start > len(records) {
		records = []*core.Record{}
	} else {
		end := start + perPage
		if end > len(records) {
			end = len(records)
		}
		records = records[start:end]
	}

	result := &search.Result{
		Items:      records,
		Page:       page,
		PerPage:    perPage,
		TotalItems: totalItems,
		TotalPages: totalPages,
	}

	event := new(core.RecordsListRequestEvent)
	event.RequestEvent = e
	event.Collection = collection
	event.Records = records
	event.Result = result

	return e.App.OnRecordsListRequest().Trigger(event, func(event *core.RecordsListRequestEvent) error {
		if err := EnrichRecords(event.RequestEvent, event.Records); err != nil {
			return firstApiError(err, event.InternalServerError("Failed to enrich records", err))
		}
		return execAfterSuccessTx(true, event.App, func() error {
			return event.JSON(200, event.Result)
		})
	})
}

func sortRecordsInMemory(records []*core.Record, fields []search.SortField) {
	sort.SliceStable(records, func(i, j int) bool {
		a := records[i].PublicExport()
		b := records[j].PublicExport()
		for _, field := range fields {
			av := nestedValue(a, field.Name)
			bv := nestedValue(b, field.Name)
			cmp := compareScalar(av, bv)
			if cmp == 0 {
				continue
			}
			if field.Direction == search.SortDesc {
				return cmp > 0
			}
			return cmp < 0
		}
		return records[i].Id < records[j].Id
	})
}

func expandComputedFilterRelations(app core.App, requestInfo *core.RequestInfo, records []*core.Record, rawFilter string) error {
	groups, err := fexpr.Parse(rawFilter)
	if err != nil {
		return err
	}
	paths := map[string]bool{}
	var collectToken func(fexpr.Token)
	var collectGroup func(fexpr.ExprGroup)
	var collectGroups func([]fexpr.ExprGroup)
	collectToken = func(token fexpr.Token) {
		if token.Type != fexpr.TokenIdentifier || strings.HasPrefix(token.Literal, "@") {
			return
		}
		parts := strings.Split(strings.SplitN(token.Literal, ":", 2)[0], ".")
		currentCollection := records[0].Collection()
		path := []string{}
		for _, part := range parts {
			rel, ok := currentCollection.Fields.GetByName(part).(*core.RelationField)
			if !ok {
				break
			}
			path = append(path, part)
			next, err := app.FindCachedCollectionByNameOrId(rel.CollectionId)
			if err != nil || next == nil {
				return
			}
			currentCollection = next
		}
		if len(path) > 0 {
			paths[strings.Join(path, ".")] = true
		}
	}
	collectGroup = func(group fexpr.ExprGroup) {
		switch item := group.Item.(type) {
		case fexpr.Expr:
			collectToken(item.Left)
			collectToken(item.Right)
		case fexpr.ExprGroup:
			collectGroup(item)
		case []fexpr.ExprGroup:
			collectGroups(item)
		}
	}
	collectGroups = func(groups []fexpr.ExprGroup) {
		for _, group := range groups {
			collectGroup(group)
		}
	}
	collectGroups(groups)
	if len(paths) == 0 {
		return nil
	}
	expandPaths := make([]string, 0, len(paths))
	for path := range paths {
		expandPaths = append(expandPaths, path)
	}
	failed := app.ExpandRecords(records, expandPaths, expandFetch(app, requestInfo))
	if len(failed) > 0 {
		for _, err := range failed {
			return err
		}
	}
	return core.PrepareVisibleComputedFields(app, requestInfo, records)
}

func evaluateInMemoryFilter(collection *core.Collection, records []*core.Record, rawFilter string, requestInfo *core.RequestInfo) ([]*core.Record, error) {
	groups, err := fexpr.Parse(rawFilter)
	if err != nil {
		return nil, err
	}
	out := make([]*core.Record, 0, len(records))
	for _, record := range records {
		match, err := evalFilterGroups(record, groups, requestInfo)
		if err != nil {
			return nil, err
		}
		if match {
			out = append(out, record)
		}
	}
	return out, nil
}

func evalFilterGroups(record *core.Record, groups []fexpr.ExprGroup, requestInfo *core.RequestInfo) (bool, error) {
	result := false
	for i, group := range groups {
		v, err := evalFilterGroup(record, group, requestInfo)
		if err != nil {
			return false, err
		}
		if i == 0 {
			result = v
		} else if group.Join == fexpr.JoinOr {
			result = result || v
		} else {
			result = result && v
		}
	}
	return result, nil
}

func evalFilterGroup(record *core.Record, group fexpr.ExprGroup, requestInfo *core.RequestInfo) (bool, error) {
	switch item := group.Item.(type) {
	case fexpr.Expr:
		return evalFilterExpr(record, item, requestInfo)
	case fexpr.ExprGroup:
		return evalFilterGroup(record, item, requestInfo)
	case []fexpr.ExprGroup:
		return evalFilterGroups(record, item, requestInfo)
	}
	return false, fmt.Errorf("unsupported filter expression")
}

func evalFilterExpr(record *core.Record, expr fexpr.Expr, requestInfo *core.RequestInfo) (bool, error) {
	left, err := evalFilterToken(record, expr.Left, requestInfo)
	if err != nil {
		return false, err
	}
	right, err := evalFilterToken(record, expr.Right, requestInfo)
	if err != nil {
		return false, err
	}
	return compareFilterValues(left, right, expr.Op), nil
}

func evalFilterToken(record *core.Record, token fexpr.Token, requestInfo *core.RequestInfo) (any, error) {
	switch token.Type {
	case fexpr.TokenText:
		return token.Literal, nil
	case fexpr.TokenNumber:
		return strconv.ParseFloat(token.Literal, 64)
	case fexpr.TokenIdentifier:
		switch strings.ToLower(token.Literal) {
		case "null":
			return nil, nil
		case "true":
			return true, nil
		case "false":
			return false, nil
		}
		return evalRecordIdentifier(record, token.Literal, requestInfo)
	default:
		return nil, fmt.Errorf("unsupported computed filter token %q", token.Literal)
	}
}

func evalRecordIdentifier(record *core.Record, identifier string, requestInfo *core.RequestInfo) (any, error) {
	if strings.HasPrefix(identifier, "@request.") {
		path := strings.Split(strings.TrimPrefix(identifier, "@request."), ".")
		var current any = map[string]any{}
		if requestInfo != nil {
			auth := any(nil)
			if requestInfo.Auth != nil {
				if requestInfo.HasSuperuserAuth() {
					requestInfo.Auth.Unhide(requestInfo.Auth.Collection().Fields.FieldNames()...).IgnoreEmailVisibility(true)
				}
				auth = requestInfo.Auth.PublicExport()
			}
			current = map[string]any{
				"context": requestInfo.Context,
				"method":  requestInfo.Method,
				"query":   requestInfo.Query,
				"headers": requestInfo.Headers,
				"body":    requestInfo.Body,
				"auth":    auth,
			}
		}
		for _, part := range path {
			name, modifier, _ := strings.Cut(part, ":")
			current = nestedValue(current, name)
			if modifier == "length" {
				current = sliceLength(current)
			}
		}
		return current, nil
	}

	parts := strings.Split(identifier, ".")
	var current any
	if len(parts) > 1 {
		if rel, ok := record.Expand()[parts[0]]; ok {
			current = relationExportValue(rel)
		} else {
			current = nestedValue(record.PublicExport(), parts[0])
		}
		parts = parts[1:]
	} else {
		current = record.PublicExport()
	}
	for _, part := range parts {
		name, modifier, _ := strings.Cut(part, ":")
		current = nestedValue(current, name)
		if modifier == "length" {
			current = sliceLength(current)
		}
	}
	return current, nil
}

func relationExportValue(raw any) any {
	switch v := raw.(type) {
	case *core.Record:
		return v.PublicExport()
	case []*core.Record:
		list := make([]map[string]any, 0, len(v))
		for _, item := range v {
			list = append(list, item.PublicExport())
		}
		return list
	default:
		return nil
	}
}

func nestedValue(raw any, key string) any {
	switch v := raw.(type) {
	case map[string]any:
		return v[key]
	case []map[string]any:
		out := make([]any, 0, len(v))
		for _, item := range v {
			out = append(out, item[key])
		}
		return out
	case []any:
		out := make([]any, 0, len(v))
		for _, item := range v {
			out = append(out, nestedValue(item, key))
		}
		return out
	default:
		return nil
	}
}

func sliceLength(v any) int {
	switch x := v.(type) {
	case []any:
		return len(x)
	case []string:
		return len(x)
	case []map[string]any:
		return len(x)
	case string:
		return len(x)
	default:
		return 0
	}
}

func compareFilterValues(left, right any, op fexpr.SignOp) bool {
	if isAnyMatch(op) {
		return compareAny(left, right, op)
	}
	cmp := compareScalar(left, right)
	switch op {
	case fexpr.SignEq:
		return equalFilterValue(left, right)
	case fexpr.SignNeq:
		return !equalFilterValue(left, right)
	case fexpr.SignLike:
		return strings.Contains(cast.ToString(left), cast.ToString(right))
	case fexpr.SignNlike:
		return !strings.Contains(cast.ToString(left), cast.ToString(right))
	case fexpr.SignLt:
		return cmp < 0
	case fexpr.SignLte:
		return cmp <= 0
	case fexpr.SignGt:
		return cmp > 0
	case fexpr.SignGte:
		return cmp >= 0
	default:
		return false
	}
}

func compareAny(left, right any, op fexpr.SignOp) bool {
	items := []any{left}
	if x, ok := left.([]any); ok {
		items = x
	}
	for _, item := range items {
		base := op
		switch op {
		case fexpr.SignAnyEq:
			base = fexpr.SignEq
		case fexpr.SignAnyNeq:
			base = fexpr.SignNeq
		case fexpr.SignAnyLike:
			base = fexpr.SignLike
		case fexpr.SignAnyNlike:
			base = fexpr.SignNlike
		case fexpr.SignAnyLt:
			base = fexpr.SignLt
		case fexpr.SignAnyLte:
			base = fexpr.SignLte
		case fexpr.SignAnyGt:
			base = fexpr.SignGt
		case fexpr.SignAnyGte:
			base = fexpr.SignGte
		}
		if compareFilterValues(item, right, base) {
			return true
		}
	}
	return false
}

func isAnyMatch(op fexpr.SignOp) bool {
	switch op {
	case fexpr.SignAnyEq, fexpr.SignAnyNeq, fexpr.SignAnyLike, fexpr.SignAnyNlike,
		fexpr.SignAnyLt, fexpr.SignAnyLte, fexpr.SignAnyGt, fexpr.SignAnyGte:
		return true
	}
	return false
}

func equalFilterValue(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return compareScalar(a, b) == 0
}

func compareScalar(a, b any) int {
	af, aok := toFloat(a)
	bf, bok := toFloat(b)
	if aok && bok {
		switch {
		case af < bf:
			return -1
		case af > bf:
			return 1
		default:
			return 0
		}
	}
	return strings.Compare(cast.ToString(a), cast.ToString(b))
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	default:
		f, err := cast.ToFloat64E(v)
		return f, err == nil
	}
}

