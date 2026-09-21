package core

import (
	"errors"
	"fmt"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/tools/search"
)

// ComputedPostFilter limits.
const (
	// ComputedPostFilterDefaultScanLimit is the default max number of rows
	// scanned from the database for a list query containing a computed filter.
	ComputedPostFilterDefaultScanLimit = 2000
	// ComputedPostFilterMaxScanLimit is the hard upper cap.
	ComputedPostFilterMaxScanLimit = 10000
)

// ErrComputedFilterScanLimit is returned when the in-memory post-filter scan
// limit is reached before enough matching records were collected.
var ErrComputedFilterScanLimit = errors.New("computed filter scan limit reached; refine the SQL portion of the filter or narrow the page")

// ComputedPostFilterResult holds the outcome of a post-filter list execution.
type ComputedPostFilterResult struct {
	Records    []*Record
	Page       int
	PerPage    int
	TotalItems int
	TotalPages int
	// Exhausted reports whether all candidate rows were examined
	// (true means TotalItems is exact).
	Exhausted bool
}

// ComputedListRequest configures a list query containing a computed filter.
type ComputedListRequest struct {
	App         App
	Collection  *Collection
	RequestInfo *RequestInfo
	// BuildBaseQuery returns a fresh base SelectQuery (without LIMIT/OFFSET)
	// with the caller-applied joins/wheres. Usually it is app.RecordQuery(collection).
	BuildBaseQuery func() *dbx.SelectQuery
	Sort           string
	Page           int
	PerPage        int
	ScanLimit      int
	Filter         *ComputedFilter
	// Expand relations applied before evaluation (optional).
	Expands []string
}

// ExecComputedPostFilter runs a list query whose filter references computed
// fields.
//
// Execution strategy:
//  1. A SQL pre-filter (the user filter with computed comparisons replaced by
//     TRUE) is applied together with the collection list rule.
//  2. Rows are fetched in keyset batches (ordered by id), hydrated
//     (expands + computed evaluation) and the ORIGINAL full filter is
//     evaluated in-memory.
//  3. Pagination is applied to the matched sequence.
//
// If any computed field evaluation fails for any scanned record, an error is
// returned and no partial/forged result list is produced.
func ExecComputedPostFilter(req ComputedListRequest) (*ComputedPostFilterResult, error) {
	if req.App == nil || req.Collection == nil || req.Filter == nil {
		return nil, errors.New("invalid computed post-filter request")
	}
	if req.BuildBaseQuery == nil {
		req.BuildBaseQuery = func() *dbx.SelectQuery {
			return req.App.RecordQuery(req.Collection)
		}
	}

	page := req.Page
	if page <= 0 {
		page = 1
	}
	perPage := req.PerPage
	if perPage <= 0 {
		perPage = search.DefaultPerPage
	}
	if perPage > search.MaxPerPage {
		perPage = search.MaxPerPage
	}
	scanLimit := req.ScanLimit
	if scanLimit <= 0 {
		scanLimit = ComputedPostFilterDefaultScanLimit
	}
	if scanLimit > ComputedPostFilterMaxScanLimit {
		scanLimit = ComputedPostFilterMaxScanLimit
	}

	sqlFilter := req.Filter.sqlSafeFilter(req.Collection)

	buildBatchQuery := func(afterId string, limit int) (*dbx.SelectQuery, error) {
		q := req.BuildBaseQuery()

		resolver := NewRecordFieldResolver(req.App, req.Collection, req.RequestInfo, req.RequestInfo.HasSuperuserAuth())

		// collection list rule
		if !req.RequestInfo.HasSuperuserAuth() && req.Collection.ListRule != nil && *req.Collection.ListRule != "" {
			ruleExpr, err := search.FilterData(*req.Collection.ListRule).BuildExpr(resolver)
			if err != nil {
				return nil, err
			}
			q.AndWhere(ruleExpr)
		}

		if sqlFilter != "" {
			expr, err := search.FilterData(sqlFilter).BuildExpr(resolver)
			if err != nil {
				return nil, fmt.Errorf("invalid sql portion of the computed filter: %w", err)
			}
			if expr != nil {
				q.AndWhere(expr)
			}
		}

		if err := resolver.UpdateQuery(q); err != nil {
			return nil, err
		}

		// SQL sort (sorting by computed fields is not supported in post-filter mode)
		for _, sf := range search.ParseSortFromString(req.Sort) {
			if sf.Name == "" {
				continue
			}
			expr, err := sf.BuildExpr(resolver)
			if err != nil {
				return nil, err
			}
			if expr != "" {
				q.AndOrderBy(expr)
			}
		}

		if afterId != "" {
			q.AndWhere(dbx.NewExp("[[id]] > {:afterId}", dbx.Params{"afterId": afterId}))
		}
		q.AndOrderBy("[[id]] ASC") // deterministic tiebreaker/keyset
		q.Limit(int64(limit))

		return q, nil
	}

	needed := (page-1)*perPage + perPage

	computeReq := NewComputeRequest(req.App, ComputedRequestConfig{
		RequestInfo:     req.RequestInfo,
		RelationFetcher: ComputedAPIRelationFetcher(req.App, req.RequestInfo),
	})

	expandFetchFunc := func(relCollection *Collection, relIds []string) ([]*Record, error) {
		return expandFetcherForComputed(req.App, req.RequestInfo, relCollection, relIds)
	}

	// dotted relation paths used in the filter must be expanded so that they
	// can be evaluated in-memory (merged with caller-requested expands)
	autoExpands := req.Filter.DottedRelationExpands(req.Collection)
	allExpands := append(append([]string{}, req.Expands...), autoExpands...)

	matches := make([]*Record, 0, needed)
	scanned := 0
	lastId := ""
	batchSize := 200
	exhausted := false

	for scanned < scanLimit {
		limit := batchSize
		if remaining := scanLimit - scanned; remaining < limit {
			limit = remaining
		}

		q, err := buildBatchQuery(lastId, limit)
		if err != nil {
			return nil, err
		}

		batch := []*Record{}
		if err := q.All(&batch); err != nil {
			return nil, err
		}

		if len(batch) == 0 {
			exhausted = true
			break
		}

		scanned += len(batch)
		lastId = batch[len(batch)-1].Id

		// apply visibility flags consistent with the regular record APIs
		// (eg. superusers see hidden fields)
		if req.RequestInfo != nil && req.RequestInfo.HasSuperuserAuth() {
			for _, r := range batch {
				r.Unhide(req.Collection.Fields.FieldNames()...)
			}
		}

		if len(allExpands) > 0 {
			failed := req.App.ExpandRecords(batch, allExpands, expandFetchFunc)
			if len(failed) > 0 {
				errsSlice := make([]error, 0, len(failed))
				for key, err := range failed {
					errsSlice = append(errsSlice, fmt.Errorf("%s: %w", key, err))
				}
				return nil, fmt.Errorf("failed to expand records for computed filter: %w", errors.Join(errsSlice...))
			}
		}

		// computed evaluation for the whole batch (shared request cache)
		if err := req.App.ComputedResolve(computeReq, batch); err != nil {
			return nil, err // no partial/forged results
		}

		for _, record := range batch {
			if req.Filter.Eval(record, computeReq) {
				matches = append(matches, record)
			}
		}

		if len(batch) < limit {
			exhausted = true
			break
		}
	}

	if !exhausted && len(matches) < needed {
		return nil, ErrComputedFilterScanLimit
	}

	start := (page - 1) * perPage
	end := start + perPage
	if start > len(matches) {
		start = len(matches)
	}
	if end > len(matches) {
		end = len(matches)
	}

	pageRecords := matches[start:end]

	totalItems := len(matches)
	totalPages := 0
	if perPage > 0 {
		totalPages = (totalItems + perPage - 1) / perPage
	}

	return &ComputedPostFilterResult{
		Records:    pageRecords,
		Page:       page,
		PerPage:    perPage,
		TotalItems: totalItems,
		TotalPages: totalPages,
		Exhausted:  exhausted,
	}, nil
}

// FilterNeedsPostProcessing is a convenience helper reporting whether the
// raw filter string references any computed field of the collection.
//
// Returns the parsed filter when post-processing is required.
func FilterNeedsPostProcessing(collection *Collection, rawFilter string) (*ComputedFilter, bool, error) {
	if rawFilter == "" {
		return nil, false, nil
	}
	parsed, err := ParseComputedFilter(rawFilter)
	if err != nil {
		return nil, false, err
	}
	if parsed.ReferencesComputedFields(collection) {
		return parsed, true, nil
	}
	return nil, false, nil
}

// expandFetcherForComputed returns an ExpandFetchFunc that applies the related
// collection ViewRule for the given request, mirroring the regular API expand
// fetch behavior.
func expandFetcherForComputed(app App, info *RequestInfo, relCollection *Collection, relIds []string) ([]*Record, error) {
	if info == nil || info.HasSuperuserAuth() {
		return app.FindRecordsByIds(relCollection.Id, relIds)
	}

	if relCollection.ViewRule == nil {
		return nil, nil // only superusers can access
	}

	ruleFunc := func(q *dbx.SelectQuery) error {
		if *relCollection.ViewRule == "" {
			return nil
		}
		resolver := NewRecordFieldResolver(app, relCollection, info, true)
		expr, err := search.FilterData(*relCollection.ViewRule).BuildExpr(resolver)
		if err != nil {
			return err
		}
		q.AndWhere(expr)
		return resolver.UpdateQuery(q)
	}

	return app.FindRecordsByIds(relCollection.Id, relIds, ruleFunc)
}
