package store

import (
	"io/fs"
	"regexp"
	"testing"
)

// destructive matches DDL that breaks the release still running during a
// rolling update: it reads and writes the old schema until its pods go.
var destructive = regexp.MustCompile(`(?is)\bDROP\s+(COLUMN|TABLE)\b|\bRENAME\b|\bALTER\s+COLUMN\s+\S+\s+(SET\s+DATA\s+)?TYPE\b|\bSET\s+NOT\s+NULL\b`)

// lastUnguarded is the newest migration from before this check.
const lastUnguarded = 11

// allowDestructive maps a destructive migration's version to the deployed
// image that already stopped using what it drops or changes.
var allowDestructive = map[int]string{}

func TestMigrationsKeepThePreviousReleaseWorking(t *testing.T) {
	files, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		v, err := migrationVersion(f)
		if err != nil {
			t.Fatal(err)
		}
		sql, err := migrations.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if m := destructive.Find(sql); m != nil && v > lastUnguarded && allowDestructive[v] == "" {
			t.Errorf("%s: %q breaks the release still serving during a rollout. Ship the code that "+
				"stops using it first, and drop or change it a release later; then add this migration's "+
				"version to allowDestructive with that release's image tag.", f, m)
		}
	}
}
