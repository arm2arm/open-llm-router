package auth

import (
	"slices"
	"strings"
)

// Group name formats, chosen by oidc.group_names. Portal login, API access
// tokens and group sync all apply the same format, so they resolve the same
// group policies.
const (
	// GroupLeaf uses the last path segment: "/org/staff" and "staff" are both
	// "staff". This matches Keycloak's group mapper with full.path off.
	GroupLeaf = "leaf"
	// GroupPath uses the full path with a leading slash: "staff" becomes
	// "/staff"; "/org/staff" stays. Use with full.path on and configure
	// policies as "/org/staff".
	GroupPath = "path"
)

// GroupNames normalizes group claims or Keycloak group paths to the format.
// In leaf format, a leaf name reached by two different paths (for example
// "/a/staff" and "/b/staff") is ambiguous and dropped, so a policy can never
// be granted through the wrong group; use path format for such realms. The
// result is sorted and duplicate-free.
func GroupNames(values []string, format string) []string {
	names := []string{}
	if format == GroupPath {
		for _, value := range values {
			value = strings.TrimSpace(value)
			if value == "" || value == "/" {
				continue
			}
			if !strings.HasPrefix(value, "/") {
				value = "/" + value
			}
			names = append(names, strings.TrimSuffix(value, "/"))
		}
	} else {
		paths := map[string]map[string]bool{} // leaf -> distinct full paths
		for _, value := range values {
			value = strings.TrimSuffix(strings.TrimSpace(value), "/")
			leaf := value[strings.LastIndex(value, "/")+1:]
			if leaf == "" {
				continue
			}
			names = append(names, leaf)
			if strings.HasPrefix(value, "/") {
				if paths[leaf] == nil {
					paths[leaf] = map[string]bool{}
				}
				paths[leaf][value] = true
			}
		}
		names = slices.DeleteFunc(names, func(name string) bool { return len(paths[name]) > 1 })
	}
	slices.Sort(names)
	return slices.Compact(names)
}
