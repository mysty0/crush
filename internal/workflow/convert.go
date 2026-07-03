package workflow

import (
	"sort"

	lua "github.com/yuin/gopher-lua"
)

// toGo converts a Lua value into a plain Go value (string, float64,
// bool, nil, []any, or map[string]any), suitable for JSON encoding or
// passing across the Runner boundary. Lua tables are treated as
// arrays when they have a contiguous 1..N integer key sequence with
// no other keys, and as maps otherwise.
func toGo(v lua.LValue) any {
	switch v := v.(type) {
	case *lua.LNilType:
		return nil
	case lua.LBool:
		return bool(v)
	case lua.LNumber:
		return float64(v)
	case lua.LString:
		return string(v)
	case *lua.LTable:
		return tableToGo(v)
	default:
		return nil
	}
}

func tableToGo(t *lua.LTable) any {
	n := t.Len()
	isArray := n > 0
	if isArray {
		// Confirm there are no non-integer keys beyond the array part.
		extra := 0
		t.ForEach(func(k, _ lua.LValue) {
			if _, ok := k.(lua.LNumber); !ok {
				extra++
			}
		})
		if extra > 0 {
			isArray = false
		}
	}

	if isArray {
		arr := make([]any, 0, n)
		for i := 1; i <= n; i++ {
			arr = append(arr, toGo(t.RawGetInt(i)))
		}
		return arr
	}

	m := make(map[string]any)
	t.ForEach(func(k, v lua.LValue) {
		m[lua.LVAsString(k)] = toGo(v)
	})
	if len(m) == 0 && n == 0 {
		// Empty table: ambiguous between {} and []. Treat as an empty
		// array, matching JSON-friendly expectations for workflow
		// results like `findings: []`.
		return []any{}
	}
	return m
}

// toLua converts a plain Go value (as produced by toGo, JSON
// unmarshaling, or Runner implementations) into a Lua value.
func toLua(L *lua.LState, v any) lua.LValue {
	switch v := v.(type) {
	case nil:
		return lua.LNil
	case bool:
		return lua.LBool(v)
	case string:
		return lua.LString(v)
	case float64:
		return lua.LNumber(v)
	case int:
		return lua.LNumber(v)
	case int64:
		return lua.LNumber(v)
	case []any:
		tbl := L.NewTable()
		for i, item := range v {
			tbl.RawSetInt(i+1, toLua(L, item))
		}
		return tbl
	case map[string]any:
		tbl := L.NewTable()
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			tbl.RawSetString(k, toLua(L, v[k]))
		}
		return tbl
	default:
		return lua.LNil
	}
}

// schemaFromLua parses a Lua table describing a JSON schema (as
// written in workflow scripts, e.g. SCOPE_SCHEMA in deep-research.lua)
// into a *Schema.
func schemaFromLua(v lua.LValue) *Schema {
	tbl, ok := v.(*lua.LTable)
	if !ok {
		return nil
	}

	s := &Schema{}
	s.Type = lua.LVAsString(tbl.RawGetString("type"))
	s.Description = lua.LVAsString(tbl.RawGetString("description"))
	s.Format = lua.LVAsString(tbl.RawGetString("format"))

	if props, ok := tbl.RawGetString("properties").(*lua.LTable); ok {
		s.Properties = make(map[string]*Schema)
		props.ForEach(func(k, val lua.LValue) {
			if sub := schemaFromLua(val); sub != nil {
				s.Properties[lua.LVAsString(k)] = sub
			}
		})
	}

	if req, ok := tbl.RawGetString("required").(*lua.LTable); ok {
		n := req.Len()
		s.Required = make([]string, 0, n)
		for i := 1; i <= n; i++ {
			s.Required = append(s.Required, lua.LVAsString(req.RawGetInt(i)))
		}
	}

	if items := tbl.RawGetString("items"); items != lua.LNil {
		s.Items = schemaFromLua(items)
	}

	if enum, ok := tbl.RawGetString("enum").(*lua.LTable); ok {
		n := enum.Len()
		s.Enum = make([]any, 0, n)
		for i := 1; i <= n; i++ {
			s.Enum = append(s.Enum, toGo(enum.RawGetInt(i)))
		}
	}

	if minItems, ok := tbl.RawGetString("minItems").(lua.LNumber); ok {
		n := int(minItems)
		s.MinItems = &n
	}
	if maxItems, ok := tbl.RawGetString("maxItems").(lua.LNumber); ok {
		n := int(maxItems)
		s.MaxItems = &n
	}
	if minLength, ok := tbl.RawGetString("minLength").(lua.LNumber); ok {
		n := int(minLength)
		s.MinLength = &n
	}
	if maxLength, ok := tbl.RawGetString("maxLength").(lua.LNumber); ok {
		n := int(maxLength)
		s.MaxLength = &n
	}
	if minimum, ok := tbl.RawGetString("minimum").(lua.LNumber); ok {
		f := float64(minimum)
		s.Minimum = &f
	}
	if maximum, ok := tbl.RawGetString("maximum").(lua.LNumber); ok {
		f := float64(maximum)
		s.Maximum = &f
	}

	return s
}
