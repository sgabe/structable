package main

import (
	"database/sql"
	"fmt"
	"io"
	"os"
	"strings"
	"text/template"

	"github.com/Masterminds/squirrel"
	"github.com/codegangsta/cli"

	_ "github.com/lib/pq"
)

const version = "DEV"

const Usage = `Read a schema and generate Structable structs.

This utility generates Structable structs be reading your database table and
generating the appropriate code.
`

const fileHeader = `package model

import (
	"github.com/Masterminds/squirrel"
	"github.com/Masterminds/structable"
	_ "github.com/lib/pq"
	"database/sql"
	"time"
)

`

const structTemplate = `// {{.StructName}} maps to database table {{.TableName}}
type {{.StructName}} struct {
	tableName string {{ann "tablename" .TableName}}
	structable.Recorder
	builder squirrel.StatementBuilderType
	{{range .Fields}}{{.}}
	{{end}}
}

// New{{.StructName}} creates a new {{.StructName}} wired to structable.
func New{{.StructName}}(db squirrel.DBProxyBeginner, flavor string) *{{.StructName}} {
	o := new({{.StructName}})
	o.Recorder = structable.New(db, flavor).Bind("{{.TableName}}", o)
	return o
}
`

type structDesc struct {
	StructName string
	TableName  string
	Fields     []string
}

func main() {
	app := cli.NewApp()
	app.Name = "schema2struct"
	app.Version = "version"
	app.Usage = Usage
	app.Action = importTables
	app.Flags = []cli.Flag{
		cli.StringFlag{
			Name:  "driver,d",
			Value: "postgres",
			Usage: "The name of the SQL driver to use.",
		},
		cli.StringFlag{
			Name:  "connection,c",
			Value: "user=$USER dbname=$USER sslmode=disable",
			Usage: "The database connection string. Environment variables are expanded.",
		},
		cli.StringFlag{
			Name:  "tables,t",
			Value: "",
			Usage: "The list of tables to generate, comma separated. If none specified, the entire schema is used.",
		},
	}

	app.Run(os.Args)
}

func driver(c *cli.Context) string {
	return c.String("driver")
}
func conn(c *cli.Context) string {
	return os.ExpandEnv(c.String("connection"))
}
func dest(c *cli.Context) io.Writer {
	return os.Stdout
}

func tableList(c *cli.Context) []string {
	z := c.String("tables")
	if z != "" {
		return strings.Split(z, ",")
	}
	return []string{}
}

func cxdie(c *cli.Context, err error) {
	fmt.Fprintf(os.Stderr, "Failed to connect to %s (type %s): %s", conn(c), driver(c), err)
	os.Exit(1)
}

var funcMap = map[string]interface{}{
	"ann": func(tag, val string) string {
		return fmt.Sprintf("`%s:\"%s\"`", tag, val)
	},
}

func importTables(c *cli.Context) {
	ttt := template.Must(template.New("st").Funcs(funcMap).Parse(structTemplate))
	cxn, err := sql.Open(driver(c), conn(c))
	if err != nil {
		cxdie(c, err)
	}
	// Many drivers defer connections until the first statement. We test
	// that here.
	if err := cxn.Ping(); err != nil {
		cxdie(c, err)
	}
	defer cxn.Close()

	// Set up Squirrel
	stmts := squirrel.NewStmtCacher(cxn)
	bldr := squirrel.StatementBuilder.RunWith(stmts)
	if driver(c) == "postgres" {
		bldr = bldr.PlaceholderFormat(squirrel.Dollar)
	}

	// Set up destination
	out := dest(c)
	fmt.Fprintln(out, fileHeader)

	tables := tableList(c)

	if len(tables) == 0 {
		tables, err = publicTables(bldr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Cannot fetch list of tables: %s\n", err)
			os.Exit(2)
		}
	}

	for _, t := range tables {
		f, err := importTable(t, bldr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to import table %s: %s", t, err)
		}

		//fmt.Fprintf(out, "%s %s %s\n", f.StructName, f.TableName, f.Fields)
		ttt.Execute(out, f)
	}
}

type column struct {
	Name, DataType string
	Max            int64
}

func publicTables(b squirrel.StatementBuilderType) ([]string, error) {
	rows, err := b.Select("table_name").From("INFORMATION_SCHEMA.TABLES").
		Where("table_schema = 'public'").Query()

	res := []string{}
	if err != nil {
		return res, err
	}

	for rows.Next() {
		var s string
		rows.Scan(&s)
		res = append(res, s)
	}

	return res, nil
}

// importTable reads a table definition and writes a corresponding struct.
// SELECT table_name, column_name, data_type, character_maximum_length
//   FROM INFORMATION_SCHEMA.COLUMNS WHERE table_name = 'goose_db_version'
func importTable(tbl string, b squirrel.StatementBuilderType) (*structDesc, error) {

	pks, err := primaryKeyField(tbl, b)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error getting primary keys: %s", err)
	}

	q := b.Select("column_name, data_type, character_maximum_length").
		From("INFORMATION_SCHEMA.COLUMNS").
		Where("table_name = ?", tbl)

	rows, err := q.Query()
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	ff := []string{}
	for rows.Next() {
		c := &column{}
		var length sql.NullInt64
		if err := rows.Scan(&c.Name, &c.DataType, &length); err != nil {
			return nil, err
		}
		c.Max = length.Int64
		ff = append(ff, structField(c, pks, tbl, b))
	}
	sd := &structDesc{
		StructName: goName(tbl),
		TableName:  tbl,
		Fields:     ff,
	}

	return sd, nil
}

func primaryKeyField(tbl string, b squirrel.StatementBuilderType) ([]string, error) {
	q := b.Select("column_name").
		From("INFORMATION_SCHEMA.KEY_COLUMN_USAGE AS c").
		LeftJoin("INFORMATION_SCHEMA.TABLE_CONSTRAINTS AS t USING(constraint_name)").
		Where("t.table_name = ? AND t.constraint_type = 'PRIMARY KEY'", tbl).
		OrderBy("ordinal_position")

	rows, err := q.Query()
	if err != nil {
		return []string{}, err
	}

	res := []string{}
	for rows.Next() {
		var s string
		rows.Scan(&s)
		res = append(res, s)
	}
	return res, nil
}

func sequentialKey(tbl, pk string, b squirrel.StatementBuilderType) bool {

	tlen := 58

	stbl := tbl
	if len(tbl) > 29 {
		stbl = tbl[0:29]
	}

	left := tlen - len(stbl)
	spk := pk
	if len(pk) > left {
		spk = pk[0:left]
	}
	seq := fmt.Sprintf("%s_%s_seq", stbl, spk)

	q := b.Select("COUNT(*)").
		From("INFORMATION_SCHEMA.SEQUENCES").
		Where("sequence_name = ?", seq)

	var num int
	if err := q.Scan(&num); err != nil {
		panic(err)
	}
	return num > 0
}

func structField(c *column, pks []string, tbl string, b squirrel.StatementBuilderType) string {
	tpl := "%s %s `stbl:\"%s\"`"
	gn := goName(c.Name)
	tt := goType(c.DataType)

	tag := c.Name
	for _, p := range pks {
		if c.Name == p {
			tag += ",PRIMARY_KEY"
			if sequentialKey(tbl, c.Name, b) {
				tag += ",SERIAL"
			}
		}
	}

	return fmt.Sprintf(tpl, gn, tt, tag)
}

// goType takes a SQL type and returns a string containin the name of a Go type.
//
// The goal is not to provide an exact match for every type, but to provide a
// safe Go representation of a SQL type.
//
// For some floating point SQL types, for example, we store them as strings
// so as not to lose precision while also not adding new types.
//
// The default type is string.
func goType(sqlType string) string {
	switch sqlType {
	case "smallint", "smallserial":
		return "int16"
	case "integer", "serial":
		return "int32"
	case "bigint", "bigserial":
		return "int"
	case "real":
		return "float32"
	case "double precision":
		return "float64"
	// Because we need to preserve base-10 precision.
	case "money":
		return "string"
	case "text", "varchar", "char", "character", "character varying", "uuid":
		return "string"
	case "bytea":
		return "[]byte"
	case "boolean":
		return "bool"
	case "timezone", "timezonetz", "date", "time":
		return "time.Time"
	case "interval":
		return "time.Duration"
	}
	return "string"
}

// Convert a SQL name to a Go name.
func goName(sqlName string) string {
	// This can definitely be done better.
	goName := strings.Replace(sqlName, "_", " ", -1)
	goName = strings.Replace(goName, ".", " ", -1)
	goName = strings.Title(goName)
	goName = strings.Replace(goName, " ", "", -1)

	return goName
}
