package redact

import (
	"strings"
	"testing"
)

func TestRedaction(t *testing.T) {
	r := New("super-secret-value-123", "short")
	cases := map[string]string{
		"token ghp_abcdefghijklmnopqrstuvwxyz0123456789 used":                  "ghp_",
		"export AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE":                        "AKIAIOSFODNN7EXAMPLE",
		"Authorization: Bearer abc.def.ghi-very-long-token-value":              "abc.def",
		"password=hunter2hunter2":                                              "hunter2",
		"cloning https://user:pa55word@github.com/org/repo.git":                "pa55word",
		"the value is super-secret-value-123!":                                 "super-secret-value-123",
		"glpat-ABCDEFGHIJKLMNOPQRSTUV":                                         "glpat-ABCDEF",
		"-----BEGIN RSA PRIVATE KEY-----\nMIIE\n-----END RSA PRIVATE KEY-----": "MIIE",
	}
	for in, leaked := range cases {
		out := r.String(in)
		if strings.Contains(out, leaked) {
			t.Errorf("%q -> %q still contains %q", in, out, leaked)
		}
		if !strings.Contains(out, Mask) {
			t.Errorf("%q -> %q missing mask", in, out)
		}
	}
	if got := r.String("a short note about passwords in general"); got != "a short note about passwords in general" {
		t.Errorf("benign text altered: %q", got)
	}
	if got := r.String("Authorization: Bearer xyz"); !strings.HasPrefix(got, "Authorization: Bearer ") {
		t.Errorf("header name should be preserved: %q", got)
	}
}
