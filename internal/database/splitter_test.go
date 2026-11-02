package database

import (
	"strings"
	"testing"
)

func TestSplitSQLStatements(t *testing.T) {
	for _, test := range []struct {
		name    string
		content string
		want    []string
		noTx    []bool
	}{
		{
			name:    "plain statements",
			content: "CREATE TABLE a (id INT);\nCREATE TABLE b (id INT);\n",
			want:    []string{"CREATE TABLE a (id INT)", "CREATE TABLE b (id INT)"},
		},
		{
			name:    "semicolon inside single-quoted string",
			content: "INSERT INTO t VALUES ('a;b');\nINSERT INTO t VALUES ('c');",
			want:    []string{"INSERT INTO t VALUES ('a;b')", "INSERT INTO t VALUES ('c')"},
		},
		{
			name:    "escaped quote inside string",
			content: "INSERT INTO t VALUES ('it''s; fine');SELECT 1;",
			want:    []string{"INSERT INTO t VALUES ('it''s; fine')", "SELECT 1"},
		},
		{
			name:    "dollar-quoted function body",
			content: "CREATE FUNCTION f() RETURNS int AS $$ BEGIN RETURN 1; END; $$ LANGUAGE plpgsql;\nSELECT f();",
			want: []string{
				"CREATE FUNCTION f() RETURNS int AS $$ BEGIN RETURN 1; END; $$ LANGUAGE plpgsql",
				"SELECT f()",
			},
		},
		{
			name:    "tagged dollar quote",
			content: "CREATE FUNCTION f() RETURNS int AS $body$ BEGIN RETURN 1; END; $body$ LANGUAGE plpgsql;",
			want:    []string{"CREATE FUNCTION f() RETURNS int AS $body$ BEGIN RETURN 1; END; $body$ LANGUAGE plpgsql"},
		},
		{
			name:    "line comment and trailing inline comment",
			content: "-- leading note\nCREATE TABLE a (id INT); -- trailing note\nSELECT 1;",
			want:    []string{"CREATE TABLE a (id INT)", "SELECT 1"},
		},
		{
			name:    "block comment with semicolon",
			content: "/* not; a split */CREATE TABLE a (id INT);",
			want:    []string{"CREATE TABLE a (id INT)"},
		},
		{
			name:    "quoted identifier with semicolon",
			content: `CREATE TABLE "weird;name" (id INT);`,
			want:    []string{`CREATE TABLE "weird;name" (id INT)`},
		},
		{
			name:    "no-transaction directive",
			content: "-- shift:no-transaction\nCREATE INDEX CONCURRENTLY idx ON t(col);\nSELECT 1;",
			want:    []string{"CREATE INDEX CONCURRENTLY idx ON t(col)", "SELECT 1"},
			noTx:    []bool{true, false},
		},
		{
			name:    "directive does not leak to the next statement",
			content: "SELECT 1;\n-- shift:no-transaction\nCREATE INDEX CONCURRENTLY i ON t(c);\nCREATE INDEX i2 ON t(c);",
			want:    []string{"SELECT 1", "CREATE INDEX CONCURRENTLY i ON t(c)", "CREATE INDEX i2 ON t(c)"},
			noTx:    []bool{false, true, false},
		},
		{
			name:    "semicolon in comment after statement is not a statement",
			content: "SELECT 1; -- was broken before\n",
			want:    []string{"SELECT 1"},
		},
		{
			name:    "escape-string literal with backslash-escaped quote",
			content: `INSERT INTO t VALUES (E'a\'; not a split');SELECT 1;`,
			want:    []string{`INSERT INTO t VALUES (E'a\'; not a split')`, "SELECT 1"},
		},
		{
			name:    "unicode-string literal with backslash-escaped quote",
			content: `INSERT INTO t VALUES (U&'a\'; still one');SELECT 2;`,
			want:    []string{`INSERT INTO t VALUES (U&'a\'; still one')`, "SELECT 2"},
		},
		{
			name:    "copy data block stays one statement",
			content: "COPY t (a, b) FROM stdin;\n1\thello;\n2\tworld\n\\.\nSELECT 1;",
			want:    []string{"COPY t (a, b) FROM stdin;\n1\thello;\n2\tworld\n\\.", "SELECT 1"},
		},
		{
			name:    "dollar tags cannot start with a digit",
			content: "SELECT $1$2;",
			want:    []string{"SELECT $1$2"},
		},
		{
			name:    "tagged dollar quote with digits after the first character",
			content: "CREATE FUNCTION f() RETURNS int AS $b2$ BEGIN RETURN 1; END; $b2$ LANGUAGE plpgsql;",
			want:    []string{"CREATE FUNCTION f() RETURNS int AS $b2$ BEGIN RETURN 1; END; $b2$ LANGUAGE plpgsql"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			statements := splitSQL(test.content)
			if len(statements) != len(test.want) {
				t.Fatalf("split into %d statements %#v, want %d", len(statements), statements, len(test.want))
			}
			for index, statement := range statements {
				if statement.sql != test.want[index] {
					t.Fatalf("statement %d = %q, want %q", index, statement.sql, test.want[index])
				}
				if statement.noTransaction && index >= len(test.noTx) {
					t.Fatalf("statement %d unexpectedly marked no-transaction", index)
				}
			}
			for index, want := range test.noTx {
				if statements[index].noTransaction != want {
					t.Fatalf("statement %d noTransaction = %v, want %v", index, statements[index].noTransaction, want)
				}
			}
		})
	}
}

func TestNormalizePage(t *testing.T) {
	if page := NormalizePage(0, -5); page.Limit != DefaultPageLimit || page.Offset != 0 {
		t.Fatalf("NormalizePage(0, -5) = %+v, want defaults", page)
	}
	if page := NormalizePage(99999, 10); page.Limit != MaxPageLimit || page.Offset != 10 {
		t.Fatalf("NormalizePage(99999, 10) = %+v, want cap %d", page, MaxPageLimit)
	}
	if page := NormalizePage(25, 7); page.Limit != 25 || page.Offset != 7 {
		t.Fatalf("NormalizePage(25, 7) = %+v, want passthrough", page)
	}
	clause := NormalizePage(0, 0).clause()
	if !strings.Contains(clause, "LIMIT") || !strings.Contains(clause, "OFFSET") {
		t.Fatalf("clause() = %q, want LIMIT/OFFSET", clause)
	}
	if (Page{}).clause() != NormalizePage(0, 0).clause() {
		t.Fatal("a zero-value Page must behave like the default page")
	}
}
