package objectstore

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestKeySanitization(t *testing.T) {
	k := Key("ten_a", "job_1", "art_1", "../../etc/passwd")
	if strings.Contains(k, "..") || !strings.HasPrefix(k, "tenants/ten_a/jobs/job_1/art_1/") {
		t.Fatalf("unsafe key %q", k)
	}
}

func TestFSRoundTrip(t *testing.T) {
	fs, err := NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	sha, n, err := fs.Put(ctx, "tenants/a/x.txt", strings.NewReader("hello"), "text/plain")
	if err != nil || n != 5 || sha != "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824" {
		t.Fatalf("put: %v %d %s", err, n, sha)
	}
	r, err := fs.Get(ctx, "tenants/a/x.txt")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r)
	r.Close()
	if string(b) != "hello" {
		t.Fatal(string(b))
	}
	if _, err := fs.Get(ctx, "../outside"); err == nil {
		t.Fatal("path traversal not rejected")
	}
	if err := fs.Delete(ctx, "tenants/a/x.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.Get(ctx, "tenants/a/x.txt"); err != ErrNotFound {
		t.Fatalf("expected not found, got %v", err)
	}
}

// AWS documented SigV4 example: GET Object with Range header.
// https://docs.aws.amazon.com/AmazonS3/latest/API/sig-v4-header-based-auth.html
func TestSigV4MatchesAWSExample(t *testing.T) {
	s := &S3{Region: "us-east-1", Bucket: "examplebucket", AccessKey: "AKIAIOSFODNN7EXAMPLE",
		SecretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		now:       func() time.Time { return time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC) }}
	req, _ := http.NewRequest(http.MethodGet, "https://examplebucket.s3.amazonaws.com/test.txt", nil)
	req.Header.Set("Range", "bytes=0-9")
	s.sign(req, emptySHA256)
	want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, SignedHeaders=host;range;x-amz-content-sha256;x-amz-date, Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
	if got := req.Header.Get("Authorization"); got != want {
		t.Fatalf("signature mismatch\n got: %s\nwant: %s", got, want)
	}
}

func TestS3AgainstFakeServer(t *testing.T) {
	objects := map[string][]byte{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
			w.WriteHeader(403)
			return
		}
		switch r.Method {
		case http.MethodPut:
			b, _ := io.ReadAll(r.Body)
			objects[r.URL.Path] = b
		case http.MethodGet:
			b, ok := objects[r.URL.Path]
			if !ok {
				w.WriteHeader(404)
				return
			}
			w.Write(b)
		case http.MethodDelete:
			delete(objects, r.URL.Path)
			w.WriteHeader(204)
		}
	}))
	defer srv.Close()
	s := &S3{Endpoint: srv.URL, Region: "us-east-1", Bucket: "artifacts", AccessKey: "k", SecretKey: "s"}
	ctx := context.Background()
	if _, _, err := s.Put(ctx, "tenants/t/jobs/j/a/spec.md", bytes.NewReader([]byte("# spec")), "text/markdown"); err != nil {
		t.Fatal(err)
	}
	if _, ok := objects["/artifacts/tenants/t/jobs/j/a/spec.md"]; !ok {
		t.Fatalf("unexpected object paths: %v", objects)
	}
	r, err := s.Get(ctx, "tenants/t/jobs/j/a/spec.md")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r)
	r.Close()
	if string(b) != "# spec" {
		t.Fatal(string(b))
	}
	if err := s.Delete(ctx, "tenants/t/jobs/j/a/spec.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, "tenants/t/jobs/j/a/spec.md"); err != ErrNotFound {
		t.Fatalf("expected not found: %v", err)
	}
}
