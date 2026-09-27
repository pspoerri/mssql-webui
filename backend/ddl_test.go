package main

import "testing"

func TestRenderDDL(t *testing.T) {
	cases := []struct {
		name string
		def  tableDef
		want string
	}{
		{"every clause", tableDef{
			Schema: "dbo", Name: "order",
			Cols: []ddlCol{
				{Name: "id", Type: "int", Identity: true, Seed: 1, Inc: 1},
				{Name: "name", Type: "nvarchar(50)", Collation: "Latin1_General_CS_AS", Nullable: true, DefaultName: "DF_order_name", Default: "('x')"},
				{Name: "total", Computed: "([a]+[b])", Persisted: true},
				{Name: "uid", Type: "int"},
			},
			Indexes: []ddlIndex{
				{Name: "PK_order", PK: true, Unique: true, Clustered: true, Cols: []string{"id"}},
				{Name: "UQ_order_name", UniqueConstraint: true, Unique: true, Cols: []string{"name"}},
				{Name: "IX_order_uid", Cols: []string{"uid DESC"}, Include: []string{"name"}, Filter: "([uid]>(0))"},
			},
			FKs:    []ddlFK{{Name: "FK_order_user", RefTable: "[dbo].[user]", Cols: []string{"uid"}, RefCols: []string{"id"}, OnDelete: "CASCADE", OnUpdate: "NO_ACTION"}},
			Checks: []ddlCheck{{Name: "CK_order", Expr: "([uid]>(0))"}},
		}, `CREATE TABLE [dbo].[order] (
    [id] int IDENTITY(1,1) NOT NULL,
    [name] nvarchar(50) COLLATE Latin1_General_CS_AS NULL CONSTRAINT [DF_order_name] DEFAULT ('x'),
    [total] AS ([a]+[b]) PERSISTED,
    [uid] int NOT NULL,
    CONSTRAINT [PK_order] PRIMARY KEY CLUSTERED ([id]),
    CONSTRAINT [UQ_order_name] UNIQUE NONCLUSTERED ([name]),
    CONSTRAINT [FK_order_user] FOREIGN KEY ([uid]) REFERENCES [dbo].[user] ([id]) ON DELETE CASCADE,
    CONSTRAINT [CK_order] CHECK ([uid]>(0))
);

CREATE NONCLUSTERED INDEX [IX_order_uid] ON [dbo].[order] ([uid] DESC) INCLUDE ([name]) WHERE ([uid]>(0));
`},
		{"heap without constraints", tableDef{
			Schema: "log", Name: "event",
			Cols: []ddlCol{{Name: "at", Type: "datetimeoffset(0)"}, {Name: "message", Type: "nvarchar(max)", Nullable: true}},
		}, `CREATE TABLE [log].[event] (
    [at] datetimeoffset(0) NOT NULL,
    [message] nvarchar(max) NULL
);
`},
		{"composite keys, set null, clustered unique index, quoting", tableDef{
			Schema: "dbo", Name: "line]x",
			Cols: []ddlCol{
				{Name: "customer_id", Type: "int", Nullable: true},
				{Name: "order_no", Type: "int", Nullable: true},
				{Name: "year", Computed: "(datepart(year,[placed]))"},
				{Name: "email", Type: "email_t", Nullable: true},
			},
			Indexes: []ddlIndex{
				{Name: "PK_line", PK: true, Unique: true, Cols: []string{"customer_id", "order_no"}},
				{Name: "CX_line", Unique: true, Clustered: true, Cols: []string{"order_no", "customer_id DESC"}},
			},
			FKs: []ddlFK{{Name: "FK_line_order", RefTable: "[dbo].[order]", Cols: []string{"customer_id", "order_no"}, RefCols: []string{"customer_id", "order_no"}, OnDelete: "SET_NULL", OnUpdate: "SET_DEFAULT"}},
		}, `CREATE TABLE [dbo].[line]]x] (
    [customer_id] int NULL,
    [order_no] int NULL,
    [year] AS (datepart(year,[placed])),
    [email] email_t NULL,
    CONSTRAINT [PK_line] PRIMARY KEY NONCLUSTERED ([customer_id], [order_no]),
    CONSTRAINT [FK_line_order] FOREIGN KEY ([customer_id], [order_no]) REFERENCES [dbo].[order] ([customer_id], [order_no]) ON DELETE SET NULL ON UPDATE SET DEFAULT
);

CREATE UNIQUE CLUSTERED INDEX [CX_line] ON [dbo].[line]]x] ([order_no], [customer_id] DESC);
`},
	}
	for _, c := range cases {
		if got := renderDDL(c.def); got != c.want {
			t.Errorf("%s: got:\n%s\nwant:\n%s", c.name, got, c.want)
		}
	}
}
