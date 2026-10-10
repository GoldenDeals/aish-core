package deepcopy

import "reflect"

// Equal reports whether a and b are equal under the model of Copy: exported
// fields are compared deeply, unexported fields shallowly (pointers, slices,
// maps, channels and funcs by identity). Unlike reflect.DeepEqual it never
// reads state behind an unexported field, so it does not race with a type
// that guards that state with its own lock.
func Equal[T any](a, b T) bool {
	return equal(reflect.ValueOf(a), reflect.ValueOf(b), map[[2]uintptr]bool{})
}

func equal(a, b reflect.Value, visited map[[2]uintptr]bool) bool {
	if !a.IsValid() || !b.IsValid() {
		return a.IsValid() == b.IsValid()
	}
	if a.Type() != b.Type() {
		return false
	}

	switch a.Kind() {
	case reflect.Pointer:
		if a.Pointer() == b.Pointer() {
			return true
		}
		if a.IsNil() || b.IsNil() {
			return false
		}
		key := [2]uintptr{a.Pointer(), b.Pointer()}
		if visited[key] {
			return true
		}
		visited[key] = true

		return equal(a.Elem(), b.Elem(), visited)

	case reflect.Interface:
		if a.IsNil() || b.IsNil() {
			return a.IsNil() == b.IsNil()
		}

		return equal(a.Elem(), b.Elem(), visited)

	case reflect.Struct:
		for i := range a.NumField() {
			if a.Type().Field(i).IsExported() {
				if !equal(a.Field(i), b.Field(i), visited) {
					return false
				}
			} else if !same(a.Field(i), b.Field(i)) {
				return false
			}
		}

		return true

	case reflect.Slice:
		if a.IsNil() != b.IsNil() || a.Len() != b.Len() {
			return false
		}

		return a.Pointer() == b.Pointer() || elementsEqual(a, b, visited)

	case reflect.Array:
		return elementsEqual(a, b, visited)

	case reflect.Map:
		if a.IsNil() != b.IsNil() || a.Len() != b.Len() {
			return false
		}
		for it := a.MapRange(); it.Next(); {
			bv := b.MapIndex(it.Key())
			if !bv.IsValid() || !equal(it.Value(), bv, visited) {
				return false
			}
		}

		return true

	case reflect.Func:
		return a.IsNil() && b.IsNil() // as reflect.DeepEqual

	default: // scalars, strings, channels
		return a.Equal(b)
	}
}

func elementsEqual(a, b reflect.Value, visited map[[2]uintptr]bool) bool {
	for i := range a.Len() {
		if !equal(a.Index(i), b.Index(i), visited) {
			return false
		}
	}

	return true
}

// same compares unexported values without looking behind references.
func same(a, b reflect.Value) bool {
	switch a.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Chan, reflect.Func, reflect.UnsafePointer:
		return a.Pointer() == b.Pointer()

	case reflect.Slice:
		return a.Pointer() == b.Pointer() && a.Len() == b.Len()

	case reflect.Interface:
		if a.IsNil() || b.IsNil() {
			return a.IsNil() == b.IsNil()
		}

		return a.Elem().Type() == b.Elem().Type() && same(a.Elem(), b.Elem())

	case reflect.Struct:
		for i := range a.NumField() {
			if !same(a.Field(i), b.Field(i)) {
				return false
			}
		}

		return true

	case reflect.Array:
		for i := range a.Len() {
			if !same(a.Index(i), b.Index(i)) {
				return false
			}
		}

		return true

	default:
		return a.Equal(b)
	}
}
