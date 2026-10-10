package objectstore

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

// S3 is a minimal S3 / S3-compatible client (AWS S3, MinIO, Ceph RGW) using
// Signature Version 4 with path-style addressing. It intentionally covers
// only Put/Get/Delete so the control plane carries no large SDK dependency.
// Workload-identity credential exchange (IRSA / EKS Pod Identity) is
// delegated to a credentials file refreshed by the platform (see docs).
type S3 struct {
	Endpoint  string // e.g. https://s3.eu-west-1.amazonaws.com or http://minio:9000
	Region    string
	Bucket    string
	AccessKey string
	SecretKey string
	// SessionToken for temporary credentials (STS).
	SessionToken string
	// SSE sets x-amz-server-side-encryption (e.g. "aws:kms" or "AES256").
	SSE    string
	Client *http.Client
	now    func() time.Time
}

// NewS3FromEnv reads standard AWS environment variables.
func NewS3FromEnv(endpoint, region, bucket string) *S3 {
	return &S3{
		Endpoint: endpoint, Region: region, Bucket: bucket,
		AccessKey:    os.Getenv("AWS_ACCESS_KEY_ID"),
		SecretKey:    os.Getenv("AWS_SECRET_ACCESS_KEY"),
		SessionToken: os.Getenv("AWS_SESSION_TOKEN"),
		SSE:          os.Getenv("ASTRION_S3_SSE"),
		Client:       &http.Client{Timeout: 5 * time.Minute},
	}
}

func (s *S3) objectURL(key string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimRight(s.Endpoint, "/"))
	if err != nil {
		return nil, err
	}
	segs := strings.Split(key, "/")
	for i, seg := range segs {
		segs[i] = uriEncode(seg, false)
	}
	u.Opaque = ""
	u.RawPath = "/" + uriEncode(s.Bucket, false) + "/" + strings.Join(segs, "/")
	u.Path = "/" + s.Bucket + "/" + key
	return u, nil
}

func (s *S3) Put(ctx context.Context, key string, r io.Reader, contentType string) (string, int64, error) {
	// Buffer to compute the checksum and a signed payload hash. Artifacts are
	// size-capped by the API, so buffering is bounded.
	data, err := io.ReadAll(r)
	if err != nil {
		return "", 0, err
	}
	sum := sha256.Sum256(data)
	u, err := s.objectURL(key)
	if err != nil {
		return "", 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u.String(), bytes.NewReader(data))
	if err != nil {
		return "", 0, err
	}
	req.ContentLength = int64(len(data))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if s.SSE != "" {
		req.Header.Set("x-amz-server-side-encryption", s.SSE)
	}
	s.sign(req, hex.EncodeToString(sum[:]))
	resp, err := s.client().Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", 0, fmt.Errorf("s3 put %s: %d %s", key, resp.StatusCode, b)
	}
	return hex.EncodeToString(sum[:]), int64(len(data)), nil
}

func (s *S3) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	u, err := s.objectURL(key)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	s.sign(req, emptySHA256)
	resp, err := s.client().Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return nil, ErrNotFound
	}
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, fmt.Errorf("s3 get %s: %d %s", key, resp.StatusCode, b)
	}
	return resp.Body, nil
}

func (s *S3) Delete(ctx context.Context, key string) error {
	u, err := s.objectURL(key)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, u.String(), nil)
	if err != nil {
		return err
	}
	s.sign(req, emptySHA256)
	resp, err := s.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("s3 delete %s: %d", key, resp.StatusCode)
	}
	return nil
}

func (s *S3) client() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	return http.DefaultClient
}

const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// sign applies AWS Signature Version 4 to req.
func (s *S3) sign(req *http.Request, payloadHash string) {
	now := time.Now().UTC()
	if s.now != nil {
		now = s.now().UTC()
	}
	amzDate := now.Format("20060102T150405Z")
	date := now.Format("20060102")
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", payloadHash)
	if s.SessionToken != "" {
		req.Header.Set("x-amz-security-token", s.SessionToken)
	}
	host := req.URL.Host
	req.Host = host

	// Canonical headers: host + all x-amz-* + content-type + range.
	headers := map[string]string{"host": host}
	for k, v := range req.Header {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "x-amz-") || lk == "content-type" || lk == "range" || lk == "content-md5" {
			headers[lk] = strings.TrimSpace(strings.Join(v, ","))
		}
	}
	keys := make([]string, 0, len(headers))
	for k := range headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var canonHeaders strings.Builder
	for _, k := range keys {
		canonHeaders.WriteString(k + ":" + headers[k] + "\n")
	}
	signedHeaders := strings.Join(keys, ";")

	canonPath := req.URL.EscapedPath()
	if canonPath == "" {
		canonPath = "/"
	}
	canonQuery := canonicalQuery(req.URL.Query())
	canonical := strings.Join([]string{req.Method, canonPath, canonQuery, canonHeaders.String(), signedHeaders, payloadHash}, "\n")

	scope := date + "/" + s.Region + "/s3/aws4_request"
	h := sha256.Sum256([]byte(canonical))
	stringToSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(h[:])
	kDate := hmacSHA256([]byte("AWS4"+s.SecretKey), date)
	kRegion := hmacSHA256(kDate, s.Region)
	kService := hmacSHA256(kRegion, "s3")
	kSigning := hmacSHA256(kService, "aws4_request")
	sig := hex.EncodeToString(hmacSHA256(kSigning, stringToSign))
	req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		s.AccessKey, scope, signedHeaders, sig))
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

func canonicalQuery(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vs := append([]string(nil), q[k]...)
		sort.Strings(vs)
		for _, v := range vs {
			parts = append(parts, uriEncode(k, true)+"="+uriEncode(v, true))
		}
	}
	return strings.Join(parts, "&")
}

// uriEncode implements the SigV4 URI encoding rules.
func uriEncode(s string, encodeSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' || c == '~':
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
