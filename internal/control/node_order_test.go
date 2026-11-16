package control

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

func TestNodeResourceOrder(t *testing.T) {
	f := newSecurityFixture(t)
	ctx := context.Background()
	for _, tc := range []struct{ path, table string }{
		{"admin/nodes", "nodes"},
		{"admin/node-groups", "node_groups"},
	} {
		t.Run(tc.table, func(t *testing.T) {
			var created []int64
			t.Cleanup(func() {
				_, _ = f.p.Exec(ctx, "DELETE FROM "+tc.table+" WHERE id=ANY($1::bigint[])", created)
			})
			for i := 0; i < 3; i++ {
				query := "INSERT INTO node_groups(name) VALUES($1) RETURNING id"
				args := []any{Secret()}
				if tc.table == "nodes" {
					query = "INSERT INTO nodes(name,token_hash) VALUES($1,$2) RETURNING id"
					args = append(args, Secret())
				}
				var id int64
				if err := f.p.QueryRow(ctx, query, args...).Scan(&id); err != nil {
					t.Fatal(err)
				}
				created = append(created, id)
			}
			read := func() []int64 {
				t.Helper()
				w := f.request(f.admin, tc.path, "GET", nil)
				securityStatus(t, w, 200)
				var rows []struct {
					ID int64 `json:"id"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
					t.Fatal(err)
				}
				ids := make([]int64, len(rows))
				for i, row := range rows {
					ids[i] = row.ID
				}
				return ids
			}
			ids := read()
			for i, j := 0, len(ids)-1; i < j; i, j = i+1, j-1 {
				ids[i], ids[j] = ids[j], ids[i]
			}
			order := tc.path + "/order"
			body := map[string]any{"ids": ids}
			securityStatus(t, f.request(f.user, order, "PUT", body), 403)
			securityStatus(t, f.request(f.admin, order, "PUT", body), 200)
			if got := read(); !reflect.DeepEqual(got, ids) {
				t.Fatalf("got order %v want %v", got, ids)
			}
			securityStatus(t, f.request(f.admin, order, "PUT", map[string]any{"ids": ids[1:]}), 409)
			securityStatus(t, f.request(f.admin, order, "PUT", map[string]any{"ids": []int64{ids[0], ids[0]}}), 400)
			securityStatus(t, f.request(f.admin, order, "PUT", map[string]any{}), 400)
			if got := read(); !reflect.DeepEqual(got, ids) {
				t.Fatalf("invalid request changed order: %v", got)
			}
			if tc.table == "nodes" {
				var added int64
				if err := f.p.QueryRow(ctx, "INSERT INTO nodes(name,token_hash) VALUES($1,$2) RETURNING id", Secret(), Secret()).Scan(&added); err != nil {
					t.Fatal(err)
				}
				created = append(created, added)
				got := read()
				if got[len(got)-1] != added {
					t.Fatalf("new node was not appended: %v", got)
				}
			}
		})
	}
}
