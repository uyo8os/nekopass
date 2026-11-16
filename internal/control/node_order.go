package control

import "net/http"

type nodeOrderInput struct {
	IDs []int64 `json:"ids"`
}

func (s *Server) orderNodes(w http.ResponseWriter, r *http.Request) {
	s.orderNodeResources(w, r, false)
}

func (s *Server) orderNodeGroups(w http.ResponseWriter, r *http.Request) {
	s.orderNodeResources(w, r, true)
}

// The whole list must be submitted: this prevents stale lists from silently
// moving or omitting newly created resources. The table lock serializes the
// validation and update with inserts, deletes, and other order changes.
func (s *Server) orderNodeResources(w http.ResponseWriter, r *http.Request, groups bool) {
	if !admin(w, r) {
		return
	}
	var v nodeOrderInput
	if !decode(w, r, &v) {
		return
	}
	if v.IDs == nil || len(v.IDs) > 100000 {
		fail(w, 400, "排序列表无效")
		return
	}
	seen := make(map[int64]bool, len(v.IDs))
	for _, id := range v.IDs {
		if id <= 0 || seen[id] {
			fail(w, 400, "排序列表包含无效或重复的 ID")
			return
		}
		seen[id] = true
	}
	ctx := r.Context()
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		s.dbError(w, err)
		return
	}
	defer tx.Rollback(ctx)
	table := "nodes"
	if groups {
		table = "node_groups"
	}
	// table is selected from fixed identifiers, never request input.
	if _, err = tx.Exec(ctx, "LOCK TABLE "+table+" IN SHARE ROW EXCLUSIVE MODE"); err != nil {
		s.dbError(w, err)
		return
	}
	var total, matched int
	err = tx.QueryRow(ctx, "SELECT count(*),count(*) FILTER (WHERE id=ANY($1::bigint[])) FROM "+table, v.IDs).Scan(&total, &matched)
	if err != nil {
		s.dbError(w, err)
		return
	}
	if total != len(v.IDs) || matched != total {
		fail(w, 409, "列表已变化，请刷新后重新排序")
		return
	}
	_, err = tx.Exec(ctx, "UPDATE "+table+" AS target SET sort_order=ordering.position FROM unnest($1::bigint[]) WITH ORDINALITY AS ordering(id,position) WHERE target.id=ordering.id", v.IDs)
	if err != nil {
		s.dbError(w, err)
		return
	}
	if err = tx.Commit(ctx); err != nil {
		s.dbError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
