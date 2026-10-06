package store

// MigrationSQL returns an embedded migration, for tests that run it again
// on rows written the old way.
func MigrationSQL(name string) string {
	b, err := migrations.ReadFile("migrations/" + name)
	if err != nil {
		panic(err)
	}
	return string(b)
}
