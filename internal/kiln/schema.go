package kiln

import _ "embed"

// Schema is the JSON Schema for kiln.yaml (`kiln schema`). Editors use it for completion and
// checks; agents use it to write a valid config the first time.
//
//go:embed kiln.schema.json
var Schema string
