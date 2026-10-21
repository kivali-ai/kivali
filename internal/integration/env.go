package integration

import "strings"

// ServerEnv is the manifest's mcp_config.env with each
// ${user_config.<key>} replaced by values[key]. A key with no value is
// replaced by the empty string: a connection field left empty, or no
// connection at all. Validate refuses any other ${...}, so none is
// left in a stored version's env.
func ServerEnv(m *Manifest, values map[string]string) map[string]string {
	out := map[string]string{}
	if m == nil {
		return out
	}
	for k, v := range m.Server.MCPConfig.Env {
		out[k] = placeholderRE.ReplaceAllStringFunc(v, func(ph string) string {
			key, ok := strings.CutPrefix(ph[2:len(ph)-1], "user_config.")
			if !ok {
				return ""
			}
			return values[key]
		})
	}
	return out
}
