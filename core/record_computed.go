package core

const (
	// internal record data keys for computed field state (never exported)
	computedValuePrefix = internalCustomFieldKeyPrefix + "computed:"
	computedErrorPrefix = internalCustomFieldKeyPrefix + "computedErr:"
)

func computedValueKey(fieldName string) string { return computedValuePrefix + fieldName }
func computedErrorKey(fieldName string) string { return computedErrorPrefix + fieldName }

// computedStoreSet stores an evaluated computed value marker on the record.
func (record *Record) computedStoreSet(fieldName string, marker *computedValueMarker) {
	record.data.Set(computedValueKey(fieldName), marker)
}

// computedStoreGet returns the stored computed value marker.
//
// The second return value reports whether the field was already evaluated.
// The third return value is the evaluated (unwrapped) value.
func (record *Record) computedStoreGet(fieldName string) (any, bool, bool) {
	v, ok := record.data.GetOk(computedValueKey(fieldName))
	if !ok {
		return nil, false, false
	}
	if mv, ok := v.(*computedValueMarker); ok {
		if mv == nil {
			// evaluated but hidden
			return nil, true, false
		}
		return mv.value, true, true
	}
	return v, true, true
}

// computedStoreSetError attaches an evaluation error to the record.
func (record *Record) computedStoreSetError(fieldName string, err error) {
	record.data.Set(computedErrorKey(fieldName), err)
}

// computedErrorsSnapshot returns a copy of all stored computed evaluation errors.
func (record *Record) computedErrorsSnapshot() map[string]error {
	result := map[string]error{}

	if record.data == nil {
		return result
	}

	for key, raw := range record.data.GetAll() {
		if len(key) <= len(computedErrorPrefix) {
			continue
		}
		if key[:len(computedErrorPrefix)] == computedErrorPrefix {
			if err, ok := raw.(error); ok {
				result[key[len(computedErrorPrefix):]] = err
			}
		}
	}

	return result
}

// hasComputedError reports whether any computed field evaluation errored.
func (record *Record) hasComputedError() bool {
	if record.data == nil {
		return false
	}
	for key := range record.data.GetAll() {
		if len(key) > len(computedErrorPrefix) && key[:len(computedErrorPrefix)] == computedErrorPrefix {
			return true
		}
	}
	return false
}

// clearComputedState drops all computed values and errors stored on the record.
func (record *Record) clearComputedState() {
	if record.data == nil {
		return
	}
	for key := range record.data.GetAll() {
		if len(key) > len(internalCustomFieldKeyPrefix) &&
			key[:len(internalCustomFieldKeyPrefix)] == internalCustomFieldKeyPrefix &&
			(len(key) >= len(computedValuePrefix) && key[:len(computedValuePrefix)] == computedValuePrefix ||
				len(key) >= len(computedErrorPrefix) && key[:len(computedErrorPrefix)] == computedErrorPrefix) {
			record.data.Remove(key)
		}
	}
}
