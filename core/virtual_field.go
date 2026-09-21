package core

// isVirtualField reports whether the given field is a non-persisted (virtual) field.
func isVirtualField(field Field) bool {
	if vf, ok := field.(VirtualField); ok {
		return vf.IsVirtual()
	}
	return false
}

// hasVirtualFields reports whether the collection has any virtual (computed) fields.
func collectionHasVirtualFields(collection *Collection) bool {
	if collection == nil {
		return false
	}
	for _, f := range collection.Fields {
		if isVirtualField(f) {
			return true
		}
	}
	return false
}
