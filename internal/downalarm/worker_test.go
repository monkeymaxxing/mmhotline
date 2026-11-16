package downalarm

import (
	"os/exec"
	"strings"
	"testing"
)

// TestWorker runs the Worker's own tests, mmhotline_down_alarm_worker_test.mjs, under
// `node --test`, so `go test ./...` covers the JavaScript too. The Worker
// needs no npm package; the tests need Node.js 22.15 or later.
func TestWorker(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed; the Worker's tests run with node --test internal/downalarm/mmhotline_down_alarm_worker_test.mjs")
	}
	output, err := exec.Command(node, "--test", "mmhotline_down_alarm_worker_test.mjs").CombinedOutput()
	if err != nil {
		t.Fatalf("node --test mmhotline_down_alarm_worker_test.mjs: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "fail 0") {
		t.Fatalf("node --test reported failures:\n%s", output)
	}
}

func TestWorkerMetadataMigratesOnlyANewWorker(t *testing.T) {
	first := WorkerMetadata("")
	migrations, ok := first["migrations"].(map[string]any)
	if !ok || migrations["new_tag"] != "v1" || migrations["new_sqlite_classes"].([]string)[0] != "Lifeline" {
		t.Fatalf("a new Worker's migrations: %v", first["migrations"])
	}
	if _, ok := WorkerMetadata("v1")["migrations"]; ok {
		t.Fatal("a Worker already at v1 got the migration again")
	}
	if WorkerMetadata("v1")["main_module"] != WorkerModuleName || !strings.Contains(WorkerSource(), "export class Lifeline extends DurableObject") {
		t.Fatal("the embedded Worker does not define the Durable Object class")
	}
}
