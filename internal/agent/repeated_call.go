package agent

import (
	"encoding/json/v2"
	"reflect"
)

// repeatedCallNote is added to a tool result when the call and its result are the same as the
// previous call's. A model that believes it changed something, say asked grep for the matching
// lines, may otherwise send the same call again and again, reading each result as a new failure.
const repeatedCallNote = "\n\n[note: this call and its result are the same as your previous tool call's]"

// toolRun is a tool call and the result it returned.
type toolRun struct {
	name, args, output string
}

// repeats reports whether r is the same call as prev, its arguments compared as JSON, and
// returned the same result.
func (r toolRun) repeats(prev toolRun) bool {
	if r.name != prev.name || r.output != prev.output {
		return false
	}
	var a, b any
	if json.Unmarshal([]byte(r.args), &a) != nil || json.Unmarshal([]byte(prev.args), &b) != nil {
		return r.args == prev.args
	}
	return reflect.DeepEqual(a, b)
}
