package kubernetes

import (
	"encoding/base64"
	"sort"
)

func encodeB64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
