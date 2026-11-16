package control

import (
	"context"
	"errors"
	"math"
	"sort"

	"github.com/jackc/pgx/v5"
	pb "github.com/nekopass/nekopass/internal/protocol"
)

type usageKey struct{ user, epoch int64 }
type grantCounters struct {
	issued, spent, released, traffic, unlimitedSpent, charged int64
	unlimitedOpen                                             bool
	changed                                                   bool
}

// applyUsageBatch locks every existing grant before validating cumulative
// reports, then applies all changes and traffic events in set-based writes.
// Reports with duplicate keys are processed in order, just as in the old loop.
func applyUsageBatch(ctx context.Context, tx pgx.Tx, nodeID int64, version int32, usage []*pb.Usage) error {
	if len(usage) == 0 {
		return nil
	}
	ids, epochs := make([]int64, 0, len(usage)), make([]int64, 0, len(usage))
	keys := make(map[usageKey]struct{}, len(usage))
	for _, u := range usage {
		charged := u.Traffic
		if version >= 25 {
			charged = u.ChargedBytes
		}
		if u.QuotaEpoch < 0 || u.Spent < 0 || u.Released < 0 || u.Traffic < 0 || u.UnlimitedSpent < 0 || u.Spent > math.MaxInt64-u.UnlimitedSpent || charged < 0 || charged > u.Spent+u.UnlimitedSpent {
			return errors.New("invalid usage counters")
		}
		k := usageKey{u.UserId, u.QuotaEpoch}
		if _, exists := keys[k]; !exists {
			keys[k] = struct{}{}
			ids = append(ids, k.user)
			epochs = append(epochs, k.epoch)
		}
	}
	rows, err := tx.Query(ctx, `SELECT g.user_id,g.quota_epoch,g.issued,g.spent,g.released,g.traffic,g.unlimited_spent,g.unlimited_open,g.charged_bytes
 FROM grants g JOIN unnest($2::bigint[],$3::bigint[]) AS x(user_id,quota_epoch) ON x.user_id=g.user_id AND x.quota_epoch=g.quota_epoch
 WHERE g.node_id=$1 ORDER BY g.user_id,g.quota_epoch FOR UPDATE OF g`, nodeID, ids, epochs)
	if err != nil {
		return err
	}
	grants := make(map[usageKey]*grantCounters, len(keys))
	for rows.Next() {
		var k usageKey
		g := new(grantCounters)
		if err = rows.Scan(&k.user, &k.epoch, &g.issued, &g.spent, &g.released, &g.traffic, &g.unlimitedSpent, &g.unlimitedOpen, &g.charged); err != nil {
			rows.Close()
			return err
		}
		grants[k] = g
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	eventUsers, eventBytes := make([]int64, 0), make([]int64, 0)
	for _, u := range usage {
		charged := u.Traffic
		if version >= 25 {
			charged = u.ChargedBytes
		}
		g := grants[usageKey{u.UserId, u.QuotaEpoch}]
		if g == nil {
			if u.Spent == 0 && u.Released == 0 && u.Traffic == 0 && u.UnlimitedSpent == 0 {
				continue
			}
			return pgx.ErrNoRows
		}
		if u.UnlimitedSpent < g.unlimitedSpent || (u.UnlimitedSpent > g.unlimitedSpent && !g.unlimitedOpen) || u.Spent < g.spent || u.Released < g.released || u.Traffic < g.traffic || charged < g.charged || u.Spent > g.issued || u.Released > g.issued-u.Spent {
			return errors.New("usage counters regressed or exceeded grant; retain the original agent state")
		}
		if u.Traffic > g.traffic {
			eventUsers = append(eventUsers, u.UserId)
			eventBytes = append(eventBytes, u.Traffic-g.traffic)
		}
		if u.Spent != g.spent || u.Released != g.released || u.Traffic != g.traffic || u.UnlimitedSpent != g.unlimitedSpent || charged != g.charged || (g.unlimitedOpen && !u.QuotaUnlimited) {
			g.changed = true
		}
		g.spent, g.released, g.traffic, g.unlimitedSpent, g.charged = u.Spent, u.Released, u.Traffic, u.UnlimitedSpent, charged
		g.unlimitedOpen = g.unlimitedOpen && u.QuotaUnlimited
	}
	ordered := make([]usageKey, 0, len(grants))
	for k, g := range grants {
		if g.changed {
			ordered = append(ordered, k)
		}
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].user != ordered[j].user {
			return ordered[i].user < ordered[j].user
		}
		return ordered[i].epoch < ordered[j].epoch
	})
	if len(ordered) > 0 {
		users, epochs, spents, released, traffic, unlimited, charged := make([]int64, 0, len(ordered)), make([]int64, 0, len(ordered)), make([]int64, 0, len(ordered)), make([]int64, 0, len(ordered)), make([]int64, 0, len(ordered)), make([]int64, 0, len(ordered)), make([]int64, 0, len(ordered))
		open := make([]bool, 0, len(ordered))
		for _, k := range ordered {
			g := grants[k]
			users = append(users, k.user)
			epochs = append(epochs, k.epoch)
			spents = append(spents, g.spent)
			released = append(released, g.released)
			traffic = append(traffic, g.traffic)
			unlimited = append(unlimited, g.unlimitedSpent)
			charged = append(charged, g.charged)
			open = append(open, g.unlimitedOpen)
		}
		_, err = tx.Exec(ctx, `UPDATE grants g SET spent=x.spent,released=x.released,traffic=x.traffic,unlimited_spent=x.unlimited_spent,unlimited_open=x.unlimited_open,charged_bytes=x.charged_bytes
 FROM unnest($2::bigint[],$3::bigint[],$4::bigint[],$5::bigint[],$6::bigint[],$7::bigint[],$8::boolean[],$9::bigint[]) AS x(user_id,quota_epoch,spent,released,traffic,unlimited_spent,unlimited_open,charged_bytes)
 WHERE g.node_id=$1 AND g.user_id=x.user_id AND g.quota_epoch=x.quota_epoch`, nodeID, users, epochs, spents, released, traffic, unlimited, open, charged)
		if err != nil {
			return err
		}
	}
	if len(eventUsers) > 0 {
		_, err = tx.Exec(ctx, `INSERT INTO usage_events(node_id,user_id,traffic_bytes) SELECT $1,x.user_id,x.traffic FROM unnest($2::bigint[],$3::bigint[]) AS x(user_id,traffic)`, nodeID, eventUsers, eventBytes)
		if err != nil {
			return err
		}
	}
	return nil
}

type ruleCounters struct {
	traffic, owner int64
	changed        bool
}

func applyRuleUsageBatch(ctx context.Context, tx pgx.Tx, nodeID int64, usage []*pb.RuleUsage) error {
	if len(usage) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(usage))
	seen := make(map[int64]struct{}, len(usage))
	for _, u := range usage {
		if u.Traffic < 0 {
			return errors.New("negative rule usage")
		}
		if _, ok := seen[u.RuleId]; !ok {
			seen[u.RuleId] = struct{}{}
			ids = append(ids, u.RuleId)
		}
	}
	rows, err := tx.Query(ctx, `SELECT r.rule_id,r.traffic,r.user_id FROM rule_usage r WHERE r.node_id=$1 AND r.rule_id=ANY($2::bigint[]) ORDER BY r.rule_id FOR UPDATE`, nodeID, ids)
	if err != nil {
		return err
	}
	rules := make(map[int64]*ruleCounters, len(ids))
	for rows.Next() {
		var id int64
		r := new(ruleCounters)
		if err = rows.Scan(&id, &r.traffic, &r.owner); err != nil {
			rows.Close()
			return err
		}
		rules[id] = r
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	eventRules, eventOwners, eventBytes := make([]int64, 0), make([]int64, 0), make([]int64, 0)
	for _, u := range usage {
		r := rules[u.RuleId]
		if r != nil && u.Traffic > r.traffic {
			eventRules = append(eventRules, u.RuleId)
			eventOwners = append(eventOwners, r.owner)
			eventBytes = append(eventBytes, u.Traffic-r.traffic)
			r.traffic = u.Traffic
			r.changed = true
		}
	}
	updated, values := make([]int64, 0, len(rules)), make([]int64, 0, len(rules))
	for id, r := range rules {
		if r.changed {
			updated = append(updated, id)
			values = append(values, r.traffic)
		}
	}
	if len(updated) > 0 {
		_, err = tx.Exec(ctx, `UPDATE rule_usage r SET traffic=x.traffic FROM unnest($2::bigint[],$3::bigint[]) AS x(rule_id,traffic) WHERE r.node_id=$1 AND r.rule_id=x.rule_id`, nodeID, updated, values)
		if err != nil {
			return err
		}
	}
	if len(eventRules) > 0 {
		_, err = tx.Exec(ctx, `INSERT INTO rule_events(node_id,rule_id,user_id,traffic_bytes) SELECT $1,x.rule_id,x.user_id,x.traffic FROM unnest($2::bigint[],$3::bigint[],$4::bigint[]) AS x(rule_id,user_id,traffic)`, nodeID, eventRules, eventOwners, eventBytes)
		if err != nil {
			return err
		}
	}
	return nil
}
