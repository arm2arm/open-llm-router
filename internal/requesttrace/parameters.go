package requesttrace

import (
	"encoding/json"
	"sort"
)

// boundParameters keeps ordinary controls first, then provider extensions in
// deterministic order. Omitted entries are counted without retaining content.
// The reserve includes the marker's longest possible integer and punctuation.
func boundParameters(parameters map[string]any) (map[string]any, bool) {
	const reserve = 64
	keys := make([]string, 0, len(parameters))
	for key := range parameters {
		keys = append(keys, key)
	}
	priority := map[string]bool{"model": true, "stream": true, "temperature": true, "top_p": true, "seed": true, "max_tokens": true, "max_completion_tokens": true, "max_output_tokens": true, "tool_choice": true}
	sort.Slice(keys, func(i, j int) bool {
		if priority[keys[i]] != priority[keys[j]] {
			return priority[keys[i]]
		}
		return keys[i] < keys[j]
	})
	out := make(map[string]any)
	size, omitted := 2, 0
	for _, key := range keys {
		entry, err := json.Marshal(map[string]any{key: parameters[key]})
		if err != nil || size+len(entry)-1 > MaxParameters-reserve {
			omitted++
			continue
		}
		size += len(entry) - 1
		out[key] = parameters[key]
	}
	if omitted > 0 {
		out["_olr_omitted_parameters"] = omitted
	}
	return out, omitted > 0
}
