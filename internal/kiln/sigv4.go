package kiln

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
	"strconv"
	"strings"
	"time"
)

// awsKey is a long-lived access key used only to sign run URLs.
type awsKey struct {
	ID     string `json:"access_key_id"`
	Secret string `json:"secret_access_key"`
}

// presignS3 returns a SigV4 query-signed URL for one object (method GET or PUT, unsigned payload).
// S3 caps expiry at 7 days. See "Authenticating Requests: Using Query Parameters" in the S3 docs.
func presignS3(k awsKey, region, bucket, key, method string, now time.Time, expires time.Duration) string {
	host := bucket + ".s3." + region + ".amazonaws.com"
	if region == "us-east-1" {
		host = bucket + ".s3.amazonaws.com"
	}
	amzDate := now.UTC().Format("20060102T150405Z")
	day := amzDate[:8]
	scope := day + "/" + region + "/s3/aws4_request"
	path := "/" + uriEncode(key, false)

	q := url.Values{}
	q.Set("X-Amz-Algorithm", "AWS4-HMAC-SHA256")
	q.Set("X-Amz-Credential", k.ID+"/"+scope)
	q.Set("X-Amz-Date", amzDate)
	q.Set("X-Amz-Expires", strconv.Itoa(int(min(expires, 7*24*time.Hour).Seconds())))
	q.Set("X-Amz-SignedHeaders", "host")
	query := canonicalQuery(q)

	canonical := strings.Join([]string{method, path, query, "host:" + host + "\n", "host", "UNSIGNED-PAYLOAD"}, "\n")
	sum := sha256.Sum256([]byte(canonical))
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(sum[:])

	sig := hmacSHA256([]byte("AWS4"+k.Secret), day)
	for _, part := range []string{region, "s3", "aws4_request", toSign} {
		sig = hmacSHA256(sig, part)
	}
	return "https://" + host + path + "?" + query + "&X-Amz-Signature=" + hex.EncodeToString(sig)
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

// canonicalQuery sorts by key and encodes per SigV4 (url.Values.Encode uses + for spaces; SigV4 wants %20).
func canonicalQuery(q url.Values) string {
	enc := q.Encode() // sorted by key
	parts := strings.Split(enc, "&")
	for i, p := range parts {
		k, v, _ := strings.Cut(p, "=")
		ku, _ := url.QueryUnescape(k)
		vu, _ := url.QueryUnescape(v)
		parts[i] = uriEncode(ku, true) + "=" + uriEncode(vu, true)
	}
	return strings.Join(parts, "&")
}

// uriEncode is SigV4's URI encoding: everything but A-Z a-z 0-9 - _ . ~ is %XX; '/' kept in paths.
func uriEncode(s string, encodeSlash bool) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		switch {
		case 'A' <= c && c <= 'Z', 'a' <= c && c <= 'z', '0' <= c && c <= '9', c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte(c)
		default:
			b.WriteString("%" + strings.ToUpper(hex.EncodeToString([]byte{c})))
		}
	}
	return b.String()
}

type httpError struct {
	Code int
	Body string
}

func (e *httpError) Error() string { return fmt.Sprintf("S3 returned HTTP %d: %s", e.Code, e.Body) }

// httpDo sends one request to a presigned URL and returns the body of a 2xx response.
func httpDo(ctx context.Context, method, u string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if body == nil {
		req.Body, req.ContentLength = http.NoBody, 0
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, &httpError{Code: resp.StatusCode, Body: string(data[:min(len(data), 300)])}
	}
	return data, nil
}
