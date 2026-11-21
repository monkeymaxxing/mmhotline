package statuspage

import (
	"os/exec"
	"strings"
	"testing"
)

// TestLocalTimeScript runs local_time_test.mjs under `node --test`, so
// `go test ./...` covers the page's JavaScript too. The script needs no npm
// package; the tests need Node.js 22.15 or later.
func TestLocalTimeScript(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed; local_time.js's tests run with node --test internal/statuspage/local_time_test.mjs")
	}
	output, err := exec.Command(node, "--test", "local_time_test.mjs").CombinedOutput()
	if err != nil {
		t.Fatalf("node --test local_time_test.mjs: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "fail 0") {
		t.Fatalf("node --test reported failures:\n%s", output)
	}
}
