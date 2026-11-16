package statuspage

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestSignRequestMatchesAWSExample checks the signer against the GET Object
// example of AWS's "Signature Version 4 header-based authentication" page,
// whose credentials, request, and signature are published there.
func TestSignRequestMatchesAWSExample(t *testing.T) {
	request, err := http.NewRequest(http.MethodGet, "https://examplebucket.s3.amazonaws.com/test.txt", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Range", "bytes=0-9")
	signRequest(request, nil, "AKIAIOSFODNN7EXAMPLE", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", "us-east-1", time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC))
	want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, SignedHeaders=host;range;x-amz-content-sha256;x-amz-date, Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
	if got := request.Header.Get("Authorization"); got != want {
		t.Fatalf("Authorization header\n got %s\nwant %s", got, want)
	}
}

func TestCheckBucketURL(t *testing.T) {
	for _, accepted := range []string{"https://0123456789abcdef.r2.cloudflarestorage.com/mmhotline-status", "http://127.0.0.1:9000/r2/mmhotline-status/"} {
		if _, err := CheckBucketURL(accepted); err != nil {
			t.Errorf("%s refused: %v", accepted, err)
		}
	}
	for _, refused := range []string{"", "http://r2.example.com/bucket", "https://r2.example.com", "https://user:secret@r2.example.com/bucket", "https://r2.example.com/bucket?x=1"} {
		if _, err := CheckBucketURL(refused); err == nil || !strings.Contains(err.Error(), "S3 API address") && !strings.Contains(err.Error(), "bucket's name") {
			t.Errorf("%q accepted or wrong error: %v", refused, err)
		}
	}
}
