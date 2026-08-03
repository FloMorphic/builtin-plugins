package castnode

// Mapping is one key/value pair the Cast / Mapping drawer collects. Key is the
// field name it lands on in the emitted object; Value is an arbitrary JSON value
// (string, number, bool, object, array). When Value is a string it may embed
// {{$.a.b}} tokens that are resolved against the live flow context before the
// object is assembled (see resolveValue).
type Mapping struct {
	Key   string `json:"key"`
	Value any    `json:"value"`
}

// RunBody is the inner `body` of the run action envelope
// ({ "_registry": {...}, "body": {...} }). Mappings is the ordered list of
// key/value pairs the node casts into a single JSON object.
type RunBody struct {
	Mappings []Mapping `json:"mappings"`
}
