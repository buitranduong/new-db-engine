package schema

// GinIndex is a GIN index on a jsonb or array column (branch jsonarr).
type GinIndex struct {
	Name  string `json:"name"`
	Col   string `json:"col"`
	Class string `json:"class,omitempty"` // jsonb_ops, jsonb_path_ops, array_ops
}
