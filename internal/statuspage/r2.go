package statuspage

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// R2Bucket reads and writes the status page's two files through R2's
// S3-compatible API, whose key can be limited to this one bucket's objects
// and whose conditional writes keep two posts from overwriting each other.
type R2Bucket struct {
	// BucketURL is the bucket's S3 API address, as Cloudflare's dashboard
	// shows it: https://ACCOUNT_ID.r2.cloudflarestorage.com/BUCKET_NAME.
	BucketURL                    string
	AccessKeyID, SecretAccessKey string
	Client                       *http.Client
	Now                          func() time.Time
}

var (
	errObjectMissing      = errors.New("object missing")
	errPreconditionFailed = errors.New("object changed")
)

// CheckBucketURL accepts an R2 bucket's S3 API address: HTTPS with the
// bucket as the only path segment. Plain HTTP is accepted only on the
// loopback interface, for a local stand-in of R2.
func CheckBucketURL(raw string) (string, error) {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	u, err := url.Parse(raw)
	loopback := false
	if err == nil {
		host := u.Hostname()
		ip := net.ParseIP(host)
		loopback = host == "localhost" || (ip != nil && ip.IsLoopback())
	}
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || !(u.Scheme == "https" || (u.Scheme == "http" && loopback)) {
		return "", errors.New("the R2 bucket URL must be the bucket's S3 API address, such as https://ACCOUNT_ID.r2.cloudflarestorage.com/BUCKET_NAME")
	}
	segments := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(segments) < 1 || segments[len(segments)-1] == "" {
		return "", errors.New("the R2 bucket URL must end with the bucket's name, such as https://ACCOUNT_ID.r2.cloudflarestorage.com/BUCKET_NAME")
	}
	return raw, nil
}

func (b *R2Bucket) client() *http.Client {
	client := http.Client{Timeout: 20 * time.Second}
	if b.Client != nil {
		client = *b.Client
	}
	// A redirect must never move a signed request elsewhere.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &client
}

func (b *R2Bucket) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now()
}

// get returns an object's content and its ETag, or errObjectMissing.
func (b *R2Bucket) get(ctx context.Context, key string) ([]byte, string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, b.BucketURL+"/"+key, nil)
	if err != nil {
		return nil, "", err
	}
	// Go asks for gzip by default, and R2 answers that with a weak ETag that
	// it then refuses in If-Match, so every post after the first would fail.
	request.Header.Set("Accept-Encoding", "identity")
	response, err := b.do(request, nil)
	if err != nil {
		return nil, "", err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return nil, "", errObjectMissing
	}
	if response.StatusCode != http.StatusOK {
		return nil, "", r2Failure("read "+key, response)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maximumStatusJSONBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("read %s from R2: %w", key, err)
	}
	if len(data) > maximumStatusJSONBytes {
		return nil, "", fmt.Errorf("%s in the bucket is larger than %d bytes", key, maximumStatusJSONBytes)
	}
	return data, response.Header.Get("ETag"), nil
}

// put uploads an object and returns the new version's ETag. A non-empty
// ifMatch writes only over that version; an empty one with key status.json
// writes only when no object exists, so two posts that read the same
// status.json cannot both win.
func (b *R2Bucket) put(ctx context.Context, key string, data []byte, contentType, ifMatch string) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, b.BucketURL+"/"+key, bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", contentType)
	// Readers see a post within about a minute even where a cache keeps the files.
	request.Header.Set("Cache-Control", "public, max-age=60")
	switch {
	case ifMatch != "":
		request.Header.Set("If-Match", ifMatch)
	case key == "status.json":
		request.Header.Set("If-None-Match", "*")
	}
	response, err := b.do(request, data)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusPreconditionFailed {
		return "", errPreconditionFailed
	}
	if response.StatusCode != http.StatusOK {
		return "", r2Failure("upload "+key, response)
	}
	return response.Header.Get("ETag"), nil
}

func (b *R2Bucket) do(request *http.Request, body []byte) (*http.Response, error) {
	signRequest(request, body, b.AccessKeyID, b.SecretAccessKey, "auto", b.now())
	response, err := b.client().Do(request)
	if err != nil {
		// The error names the address, never the signed header.
		return nil, fmt.Errorf("cannot reach R2 at %s: %w", b.BucketURL, errors.Unwrap(err))
	}
	return response, nil
}

// r2Failure reads the S3 error code and message R2 answered with.
func r2Failure(action string, response *http.Response) error {
	var answer struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}
	data, _ := io.ReadAll(io.LimitReader(response.Body, 16<<10))
	_ = xml.Unmarshal(data, &answer)
	reason := strings.TrimSpace(answer.Code + " " + answer.Message)
	if reason == "" {
		reason = "no reason given"
	}
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return fmt.Errorf("R2 refused to %s (HTTP %d, %s); check whether the R2 access key has read and write permissions for the status page's bucket", action, response.StatusCode, reason)
	}
	return fmt.Errorf("R2 could not %s (HTTP %d, %s)", action, response.StatusCode, reason)
}

// signRequest adds AWS Signature Version 4 headers for the S3 service, as
// R2's S3 API requires. It signs the host and every header already set on
// the request, and the SHA-256 of the body.
// https://docs.aws.amazon.com/AmazonS3/latest/API/sig-v4-header-based-auth.html
func signRequest(request *http.Request, body []byte, accessKeyID, secretAccessKey, region string, now time.Time) {
	now = now.UTC()
	amzDate := now.Format("20060102T150405Z")
	day := now.Format("20060102")
	payload := sha256.Sum256(body)
	payloadHash := hex.EncodeToString(payload[:])
	request.Header.Set("X-Amz-Date", amzDate)
	request.Header.Set("X-Amz-Content-Sha256", payloadHash)

	headers := map[string]string{"host": request.URL.Host}
	for name, values := range request.Header {
		headers[strings.ToLower(name)] = strings.Join(strings.Fields(strings.Join(values, ",")), " ")
	}
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	sort.Strings(names)
	var canonicalHeaders strings.Builder
	for _, name := range names {
		canonicalHeaders.WriteString(name + ":" + headers[name] + "\n")
	}
	signedHeaders := strings.Join(names, ";")
	path := request.URL.EscapedPath()
	if path == "" {
		path = "/"
	}
	canonicalRequest := strings.Join([]string{request.Method, path, request.URL.Query().Encode(), canonicalHeaders.String(), signedHeaders, payloadHash}, "\n")
	scope := day + "/" + region + "/s3/aws4_request"
	requestHash := sha256.Sum256([]byte(canonicalRequest))
	stringToSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(requestHash[:])
	key := []byte("AWS4" + secretAccessKey)
	for _, part := range []string{day, region, "s3", "aws4_request"} {
		key = hmacSHA256(key, part)
	}
	signature := hex.EncodeToString(hmacSHA256(key, stringToSign))
	request.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+accessKeyID+"/"+scope+", SignedHeaders="+signedHeaders+", Signature="+signature)
}

func hmacSHA256(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(data))
	return mac.Sum(nil)
}
