package store

import (
	"crypto/sha256"
	"encoding/hex"
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

// migrationSums are the first 16 hex digits of each migration's SHA-256. A
// migration runs once per database, by version: one edited after it shipped
// never runs where it already did, so the code and those databases part.
// Add a line with each new migration; never change one.
var migrationSums = map[int]string{
	1:  "1bde0c27c89e3565",
	2:  "db06f9103464ee8f",
	3:  "8515406e2c8494d9",
	4:  "7a2eac64c643954c",
	5:  "0becc9388f9a818e",
	6:  "4ae12a3337243fc5",
	7:  "9a6051a564677c03",
	8:  "1a10bad6d2a5f90b",
	9:  "befe3f2d07248ed1",
	10: "92605fc979449027",
	11: "fc1e4aba36eff79f",
	12: "b156ed7943bf76b2",
	13: "ec365b6323f3b1b7",
	14: "c359f3445aa843cb",
	15: "997c2357ec0fe41f",
	16: "8a193bfc9df42efa",
}

func TestShippedMigrationsDontChange(t *testing.T) {
	files, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != len(migrationSums) {
		t.Errorf("%d migrations, %d sums: add the new migration's sum to migrationSums", len(files), len(migrationSums))
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
		sum := sha256.Sum256(sql)
		if got := hex.EncodeToString(sum[:])[:16]; got != migrationSums[v] {
			t.Errorf("%s: sum %s, want %s. Don't edit a migration that may have run; add a new one", f, got, migrationSums[v])
		}
	}
}
