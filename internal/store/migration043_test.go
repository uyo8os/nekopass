package store

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestIngressGroupMigration(t *testing.T) {
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
	namespace := fmt.Sprintf("ingress_migration_%d", time.Now().UnixNano())
	exec("CREATE SCHEMA " + namespace)
	exec("SET LOCAL search_path TO " + namespace)
	for _, sql := range []string{schema, migration002, migration003, migration004, migration005, migration006, migration007, migration008, migration009, migration010, migration011, migration012, migration013, migration014, migration015, migration016, migration017, migration018, migration019, migration020, migration021, migration022, migration023, migration024, migration025, migration026, migration027, migration028, migration029, migration030, migration031, migration032, migration033, migration034, migration035, migration036, migration037, migration038, migration039, migration040, migration041, migration042} {
		exec(sql)
	}
	exec(`INSERT INTO users(id,username,password_hash) VALUES(1,'ingress-migration@example.test','fixture');
INSERT INTO nodes(id,name,token_hash,port_min,port_max) VALUES(1,'one','one',80,65535),(2,'two','two',80,65535),(3,'three','three',80,65535);
INSERT INTO node_groups(id,name) VALUES(1,'group');
INSERT INTO node_group_members(group_id,node_id,role) VALUES(1,1,'active'),(1,2,'disabled');
INSERT INTO rules(id,user_id,node_id,listen_port,target_host,target_port) VALUES(1,1,1,8080,'127.0.0.1',8080), (2,1,3,8080,'127.0.0.1',8080);`)
	exec("SET CONSTRAINTS ALL IMMEDIATE")
	exec(migration043)
	exec("UPDATE rules SET ingress_group_id=1 WHERE id=1")
	var ok bool
	if err := tx.QueryRow(ctx, `SELECT (SELECT count(*)=2 FROM rule_ingress_nodes WHERE rule_id=1)
 AND (SELECT count(*)=2 FROM rule_ports WHERE rule_id=1 AND listen_port=8080)
 AND (SELECT count(*)=2 FROM rule_usage WHERE rule_id=1)`).Scan(&ok); err != nil || !ok {
		t.Fatal("group rule not deployed on each member", err)
	}
	exec("SAVEPOINT conflict")
	if _, err := tx.Exec(ctx, "INSERT INTO node_group_members(group_id,node_id) VALUES(1,3)"); err == nil {
		t.Fatal("conflicting member was accepted")
	}
	exec("ROLLBACK TO SAVEPOINT conflict")
	exec("DELETE FROM node_group_members WHERE group_id=1 AND node_id=2")
	if err := tx.QueryRow(ctx, `SELECT (SELECT count(*)=1 FROM rule_ports WHERE rule_id=1) AND (SELECT count(*)=2 FROM rule_usage WHERE rule_id=1)`).Scan(&ok); err != nil || !ok {
		t.Fatal("removal did not release port or preserve history", err)
	}
	exec("UPDATE nodes SET ingress_enabled=false WHERE id=3")
	exec("INSERT INTO node_group_members(group_id,node_id) VALUES(1,3)")
	if err := tx.QueryRow(ctx, "SELECT NOT EXISTS(SELECT 1 FROM rule_ports WHERE rule_id=1 AND node_id=3)").Scan(&ok); err != nil || !ok {
		t.Fatal("exit-only node reserved an ingress port", err)
	}
	exec("SAVEPOINT conflict_on_enable")
	if _, err := tx.Exec(ctx, "UPDATE nodes SET ingress_enabled=true WHERE id=3"); err == nil {
		t.Fatal("conflicting ingress activation was accepted")
	}
	exec("ROLLBACK TO SAVEPOINT conflict_on_enable")
	exec("UPDATE rules SET listen_port=8081 WHERE id=2")
	exec("UPDATE nodes SET ingress_enabled=true WHERE id=3")
	if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM rule_ports WHERE rule_id=1 AND node_id=3 AND listen_port=8080)").Scan(&ok); err != nil || !ok {
		t.Fatal("new member did not receive port reservation", err)
	}
	if err := tx.QueryRow(ctx, "SELECT r.config_revision=v.value FROM rules r CROSS JOIN revision v WHERE r.id=1 AND v.id=1").Scan(&ok); err != nil || !ok {
		t.Fatal("membership change did not publish rule revision", err)
	}
	for _, sql := range []string{"UPDATE nodes SET enabled=false WHERE id=3", "UPDATE nodes SET enabled=true WHERE id=3", "UPDATE node_groups SET enabled=false WHERE id=1", "UPDATE node_groups SET enabled=true WHERE id=1"} {
		var oldRevision int64
		if err := tx.QueryRow(ctx, "SELECT config_revision FROM rules WHERE id=1").Scan(&oldRevision); err != nil {
			t.Fatal(err)
		}
		exec(sql)
		if err := tx.QueryRow(ctx, "SELECT r.config_revision=v.value AND r.config_revision>$1 FROM rules r CROSS JOIN revision v WHERE r.id=1 AND v.id=1", oldRevision).Scan(&ok); err != nil || !ok {
			t.Fatalf("%s did not advance the rule revision: %v", sql, err)
		}
	}
	// Upgrade a live v43 group and ensure existing group rules and reservations
	// continue to work without requiring administrators to classify the group.
	exec(migration044)
	if err := tx.QueryRow(ctx, `SELECT (SELECT max(version)=44 FROM schema_version)
 AND (SELECT group_type='legacy' AND connection_address='' FROM node_groups WHERE id=1)
 AND (SELECT ingress_group_id=1 FROM rules WHERE id=1)
 AND (SELECT count(*)=2 FROM rule_ports WHERE rule_id=1)`).Scan(&ok); err != nil || !ok {
		t.Fatal("v44 did not preserve existing group and rule", err)
	}
	exec(migration045)
	if err := tx.QueryRow(ctx, `SELECT (SELECT max(version)=45 FROM schema_version)
 AND (SELECT bool_and(sort_order IS NULL) FROM nodes)
 AND (SELECT ingress_group_id=1 FROM rules WHERE id=1)`).Scan(&ok); err != nil || !ok {
		t.Fatal("v45 did not preserve existing order and rule", err)
	}
}
