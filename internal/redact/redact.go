// Package redact removes secrets from text before it is persisted (AUTH-002,
// SRS 22). It combines exact-match redaction of known secret values with
// pattern-based detection of common credential formats.
package redact

import (
	"regexp"
	"sort"
	"strings"
	"sync"
)

const Mask = "[REDACTED]"

var patterns = []*regexp.Regexp{
	regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{30,255}`),                          // GitHub tokens
	regexp.MustCompile(`github_pat_[A-Za-z0-9_]{20,255}`),                        // GitHub fine-grained PAT
	regexp.MustCompile(`glpat-[A-Za-z0-9_\-]{20,}`),                              // GitLab PAT
	regexp.MustCompile(`\b(AKIA|ASIA)[A-Z0-9]{16}\b`),                            // AWS access key id
	regexp.MustCompile(`sk-ant-[A-Za-z0-9_\-]{20,}`),                             // Anthropic keys
	regexp.MustCompile(`\bsk-[A-Za-z0-9]{32,}\b`),                                // generic sk- keys
	regexp.MustCompile(`xox[baprs]-[A-Za-z0-9\-]{10,}`),                          // Slack tokens
	regexp.MustCompile(`(?i)(authorization:\s*(bearer|basic|token)\s+)[^\s"']+`), // auth headers
	regexp.MustCompile(`(?i)\b(bearer)\s+[A-Za-z0-9\-._~+/]{20,}=*`),
	regexp.MustCompile(`eyJ[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}`), // JWTs
	regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`),
	regexp.MustCompile(`(?i)((?:password|passwd|secret|api[_-]?key|access[_-]?token)\s*[=:]\s*)("[^"]+"|'[^']+'|[^\s,;]+)`),
	regexp.MustCompile(`(https?://)[^/\s:@]+:[^/\s@]+@`), // credentials embedded in URLs
}

// Redactor is safe for concurrent use.
type Redactor struct {
	mu      sync.RWMutex
	secrets []string
}

func New(secrets ...string) *Redactor {
	r := &Redactor{}
	r.Add(secrets...)
	return r
}

// Add registers exact secret values. Very short values are ignored to avoid
// destroying ordinary text.
func (r *Redactor) Add(secrets ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range secrets {
		s = strings.TrimSpace(s)
		if len(s) >= 8 {
			r.secrets = append(r.secrets, s)
		}
	}
	// Longest first so overlapping secrets are fully masked.
	sort.Slice(r.secrets, func(i, j int) bool { return len(r.secrets[i]) > len(r.secrets[j]) })
}

func (r *Redactor) String(s string) string {
	r.mu.RLock()
	for _, sec := range r.secrets {
		if strings.Contains(s, sec) {
			s = strings.ReplaceAll(s, sec, Mask)
		}
	}
	r.mu.RUnlock()
	for i, p := range patterns {
		switch i {
		case 7, 11: // keep the key/header name, mask the value
			s = p.ReplaceAllString(s, "${1}"+Mask)
		case 12:
			s = p.ReplaceAllString(s, "${1}"+Mask+"@")
		default:
			s = p.ReplaceAllString(s, Mask)
		}
	}
	return s
}
