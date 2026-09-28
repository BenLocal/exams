// Package migrations embeds the SQL schema files so the binary can apply them
// without needing the directory shipped alongside it.
//
// Files are applied in filename order by `exams migrate`. A file whose name
// ends in `.optional.sql` is best-effort: if it fails (typically because the
// database user may not CREATE EXTENSION) the run warns and continues.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
