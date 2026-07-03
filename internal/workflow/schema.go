package workflow

// Schema is a minimal JSON-schema representation used to constrain
// structured agent() output. It intentionally mirrors the subset of
// JSON Schema that fantasy/schema.Schema supports, but is defined
// independently so the engine package has no dependency on fantasy.
// internal/agent converts Schema to fantasy.Schema at the Runner
// boundary.
type Schema struct {
	Type        string             `json:"type,omitempty"`
	Properties  map[string]*Schema `json:"properties,omitempty"`
	Required    []string           `json:"required,omitempty"`
	Items       *Schema            `json:"items,omitempty"`
	Description string             `json:"description,omitempty"`
	Enum        []any              `json:"enum,omitempty"`
	Format      string             `json:"format,omitempty"`
	Minimum     *float64           `json:"minimum,omitempty"`
	Maximum     *float64           `json:"maximum,omitempty"`
	MinLength   *int               `json:"minLength,omitempty"`
	MaxLength   *int               `json:"maxLength,omitempty"`
	// MinItems and MaxItems are accepted from workflow scripts for
	// documentation purposes (e.g. "angles: 3-6") but are not
	// enforced by the coercion pass, since fantasy.Schema has no
	// equivalent field. Scripts should treat them as guidance to the
	// model via prompt text, not a hard constraint.
	MinItems *int `json:"minItems,omitempty"`
	MaxItems *int `json:"maxItems,omitempty"`
}
