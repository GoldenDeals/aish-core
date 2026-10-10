package aish

import "reflect"

// cloneEvent returns a deep copy of ev for a Handle or Observe hook.
//
// Exported fields are copied recursively: structs, pointers, slices, maps,
// arrays and interfaces. Unexported fields are copied shallowly, so private
// state (such as a Stream buffer) stays shared; a type that needs something
// else implements Clone() returning its own type. Pointer cycles are kept.
func cloneEvent(ev Event) Event {
	if ev == nil {
		return nil
	}
	out, _ := cloneValue(reflect.ValueOf(ev), map[uintptr]reflect.Value{}).Interface().(Event)

	return out
}

func cloneValue(v reflect.Value, seen map[uintptr]reflect.Value) reflect.Value {
	if c, ok := callClone(v); ok {
		return c
	}

	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return v
		}
		if c, ok := seen[v.Pointer()]; ok {
			return c
		}
		c := reflect.New(v.Type().Elem())
		seen[v.Pointer()] = c
		c.Elem().Set(cloneValue(v.Elem(), seen))

		return c

	case reflect.Interface:
		if v.IsNil() {
			return v
		}
		c := reflect.New(v.Type()).Elem()
		c.Set(cloneValue(v.Elem(), seen))

		return c

	case reflect.Struct:
		c := reflect.New(v.Type()).Elem()
		c.Set(v) // unexported fields stay shallow
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				c.Field(i).Set(cloneValue(v.Field(i), seen))
			}
		}

		return c

	case reflect.Slice:
		if v.IsNil() {
			return v
		}
		c := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		if isFlat(v.Type().Elem()) {
			reflect.Copy(c, v)

			return c
		}
		for i := range v.Len() {
			c.Index(i).Set(cloneValue(v.Index(i), seen))
		}

		return c

	case reflect.Array:
		c := reflect.New(v.Type()).Elem()
		for i := range v.Len() {
			c.Index(i).Set(cloneValue(v.Index(i), seen))
		}

		return c

	case reflect.Map:
		if v.IsNil() {
			return v
		}
		c := reflect.MakeMapWithSize(v.Type(), v.Len())
		for it := v.MapRange(); it.Next(); {
			c.SetMapIndex(it.Key(), cloneValue(it.Value(), seen))
		}

		return c

	default: // scalars, strings, funcs, chans: copied by value
		return v
	}
}

// callClone uses a Clone() T method of v's own type T when there is one.
func callClone(v reflect.Value) (reflect.Value, bool) {
	if !v.CanInterface() || (v.Kind() == reflect.Pointer && v.IsNil()) {
		return reflect.Value{}, false
	}
	m := v.MethodByName("Clone")
	if !m.IsValid() {
		return reflect.Value{}, false
	}
	if t := m.Type(); t.NumIn() != 0 || t.NumOut() != 1 || t.Out(0) != v.Type() {
		return reflect.Value{}, false
	}

	return m.Call(nil)[0], true
}

// isFlat reports whether values of t contain no references, so a shallow
// copy is a deep copy.
func isFlat(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64, reflect.Complex64, reflect.Complex128, reflect.String:
		return true
	default:
		return false
	}
}
