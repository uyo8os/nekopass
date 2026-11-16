package store

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestPortRangeMigration(t *testing.T) {
	dsn := os.Getenv("NEKOPASS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires a dedicated PostgreSQL test database")
	}
	config, err := pgx.ParseConfig(dsn)
	if err != nil || !strings.HasSuffix(config.Database, "_test") {
		t.Fatal("refusing migration test outside a dedicated test database")
	}
	ctx := context.Background()
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	exec := func(sql string) {
		t.Helper()
		if _, err := tx.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	namespace := fmt.Sprintf("port_migration_%d", time.Now().UnixNano())
	exec("CREATE SCHEMA " + namespace)
	exec("SET LOCAL search_path TO " + namespace)
	for _, sql := range []string{schema, migration002, migration003, migration004, migration005, migration006, migration007, migration008, migration009, migration010, migration011, migration012, migration013, migration014, migration015, migration016, migration017, migration018, migration019, migration020, migration021, migration022, migration023, migration024, migration025, migration026, migration027, migration028, migration029, migration030, migration031, migration032, migration033, migration034, migration035, migration036, migration037, migration038, migration039, migration040, migration041} {
		exec(sql)
	}
	exec(`INSERT INTO users(id,username,password_hash) VALUES(1,'migration@example.test','fixture');
INSERT INTO nodes(id,name,token_hash) VALUES(1,'migration','fixture');
INSERT INTO rules(id,user_id,node_id,listen_port,target_host,target_port) VALUES(1,1,1,1024,'127.0.0.1',8080);`)
	// Existing rows were committed before a real upgrade. Flush deferred foreign-key
	// checks before the DDL while retaining the isolated test transaction.
	exec("SET CONSTRAINTS ALL IMMEDIATE")
	exec(migration042)
	exec("UPDATE nodes SET port_min=80 WHERE id=1")
	exec("UPDATE rules SET listen_port=80 WHERE id=1")
	exec(`INSERT INTO node_tls_inbounds(id,node_id,config,port) VALUES(1,1,'{}',443);
INSERT INTO rules(id,user_id,node_id,listen_port,target_host,target_port,tls_inbound_id,sni_names)
 VALUES(2,1,1,0,'127.0.0.1',8080,1,'["example.test"]');`)
	var preserved bool
	if err := tx.QueryRow(ctx, `SELECT (SELECT port_min=80 FROM nodes WHERE id=1) AND
 (SELECT listen_port=80 FROM rules WHERE id=1) AND
 (SELECT listen_port=0 FROM rules WHERE id=2) AND
 EXISTS(SELECT 1 FROM rule_ports WHERE rule_id=1 AND listen_port=80) AND
 EXISTS(SELECT 1 FROM rule_ports WHERE tls_inbound_id=1 AND listen_port=443) AND
 (SELECT max(version)=42 FROM schema_version)`).Scan(&preserved); err != nil || !preserved {
		t.Fatal("migration did not preserve and update existing data", err)
	}
	for _, query := range []string{"UPDATE nodes SET port_min=79 WHERE id=1", "UPDATE rules SET listen_port=79 WHERE id=1", "UPDATE rules SET listen_port=0 WHERE id=1"} {
		exec("SAVEPOINT invalid_port")
		_, err := tx.Exec(ctx, query)
		if pgErr, ok := err.(*pgconn.PgError); !ok || pgErr.Code != "23514" {
			t.Errorf("expected check violation for %s; got %v", query, err)
		}
		exec("ROLLBACK TO SAVEPOINT invalid_port")
	}
}
