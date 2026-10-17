package migrations

import (
	"fmt"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

func TestMigrationNumericPrefixesAreUnique(t *testing.T) {
	files := migrationFilesForLint(t, "*.up.sql")

	// Migrations through 128 contain historical duplicate numeric prefixes.
	// The deployed fork and v0.6.3 independently allocated 573–582. Preserve
	// their full migration names to avoid replaying deployed DDL (see FORK.md).
	// Only these exact historical pairs may share a number; future additions
	// must retain unique numeric prefixes.
	const firstUniqueMigrationNumber = 129
	deployedForkPairs := map[string]string{
		"573_issue_workflow":                            "573_task_status_notification",
		"574_issue_workflow_pkey_index":                 "574_task_status_notification_subscription_idx",
		"575_issue_workflow_primary_key":                "575_task_status_notification_event_pending_idx",
		"576_issue_workflow_workspace_name_index":       "576_task_status_notification_event_issue_idx",
		"577_project_workflow_id":                       "577_task_status_notification_dedup_idx",
		"578_agent_task_workflow_step":                  "578_task_status_notification_outbox_idx",
		"579_task_status_notification_compose_task_idx": "579_wakeup_skip_handed_to_agent",
		"581_issue_workflow_is_default":                 "581_task_status_notification_session_idx",
		"582_issue_workflow_default_index":              "582_task_status_notification_message_idx",
	}
	stemByNumber := make(map[int]string)
	for _, file := range files {
		stem, _, ok := splitMigrationFilename(filepath.Base(file))
		if !ok {
			continue
		}
		prefix, _, ok := strings.Cut(stem, "_")
		if !ok {
			continue
		}
		number, err := strconv.Atoi(prefix)
		if err != nil || number < firstUniqueMigrationNumber {
			continue
		}
		if previous, exists := stemByNumber[number]; exists {
			if deployedForkPairs[previous] == stem {
				continue
			}
			t.Errorf("migrations %s and %s share numeric prefix %s", previous, stem, prefix)
			continue
		}
		stemByNumber[number] = stem
	}
}

func TestMigrationFilesHaveMatchingDirections(t *testing.T) {
	files := migrationFilesForLint(t, "*.sql")

	directionsByStem := make(map[string]map[string]bool)
	for _, file := range files {
		stem, direction, ok := splitMigrationFilename(filepath.Base(file))
		if !ok {
			continue
		}
		if directionsByStem[stem] == nil {
			directionsByStem[stem] = make(map[string]bool)
		}
		directionsByStem[stem][direction] = true
	}

	for stem, directions := range directionsByStem {
		if !directions["up"] || !directions["down"] {
			t.Errorf("migration %s must have both .up.sql and .down.sql files", stem)
		}
	}
}

func migrationFilesForLint(t *testing.T, pattern string) []string {
	t.Helper()

	dir := realMigrationsDir(t)
	files, err := filepath.Glob(filepath.Join(dir, pattern))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatalf("no migration files matched %s in %s", pattern, dir)
	}
	sort.Strings(files)
	return files
}

func realMigrationsDir(t *testing.T) string {
	t.Helper()

	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve migration lint test path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(self), "..", "..", "migrations"))
}

func splitMigrationFilename(name string) (stem, direction string, ok bool) {
	for _, candidateDirection := range []string{"up", "down"} {
		suffix := fmt.Sprintf(".%s.sql", candidateDirection)
		if strings.HasSuffix(name, suffix) {
			return strings.TrimSuffix(name, suffix), candidateDirection, true
		}
	}
	return "", "", false
}
