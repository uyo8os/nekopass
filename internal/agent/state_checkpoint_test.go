package agent

import (
	"path/filepath"
	"sync"
	"testing"

	pb "github.com/nekopass/nekopass/internal/protocol"
	bolt "go.etcd.io/bbolt"
	"google.golang.org/protobuf/proto"
)

func stateTransactionID(t *testing.T, s *State) int {
	t.Helper()
	var id int
	if err := s.db.View(func(tx *bolt.Tx) error { id = tx.ID(); return nil }); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestStateCheckpointSkipsUnchangedValuesAndKeepsChangedValuesDurable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := OpenState(path)
	if err != nil {
		t.Fatal(err)
	}
	users := map[int64]DiskUser{1: {Metered: true, Issued: 100, Spent: 50, Traffic: 30, Charged: 30}}
	rules := map[int64]int64{2: 30}
	retired := map[string]DiskUser{"1:0": {Metered: true, Issued: 20, Spent: 10, Traffic: 10, Charged: 10}}
	config := &pb.ControlMessage{Revision: 1, Node: &pb.NodeConfig{Enabled: true}, Users: []*pb.UserPolicy{{Id: 1, Enabled: true}}}
	if err := s.SaveConfig(config); err != nil {
		t.Fatal(err)
	}
	if err := s.checkpoint(users, rules, retired); err != nil {
		t.Fatal(err)
	}
	committed := stateTransactionID(t, s)
	for i := 0; i < 5; i++ {
		if err := s.SaveConfig(proto.Clone(config).(*pb.ControlMessage)); err != nil {
			t.Fatal(err)
		}
		if err := s.checkpoint(users, rules, retired); err != nil {
			t.Fatal(err)
		}
	}
	if id := stateTransactionID(t, s); id != committed {
		t.Fatalf("unchanged heartbeat wrote transactions: %d -> %d", committed, id)
	}

	// The revision is unchanged: changed policy and counters must still be saved.
	config.Users[0].Enabled = false
	if err := s.SaveConfig(config); err != nil {
		t.Fatal(err)
	}
	d := users[1]
	d.Traffic, d.Charged = 40, 40
	users[1], rules[2] = d, 40
	if err := s.checkpoint(users, rules, retired); err != nil {
		t.Fatal(err)
	}
	if id := stateTransactionID(t, s); id != committed+2 {
		t.Fatalf("expected config and checkpoint commits, got %d -> %d", committed, id)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.checkpoint(users, rules, retired); err == nil {
		t.Fatal("closed state accepted unchanged checkpoint")
	}
	s, err = OpenState(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	loadedConfig, err := s.Config()
	if err != nil || loadedConfig.Users[0].Enabled {
		t.Fatalf("policy was not durable: %v %v", loadedConfig, err)
	}
	loadedUsers, err := s.LoadUsers()
	if err != nil || loadedUsers[1] != users[1] {
		t.Fatalf("usage was not durable: %v %v", loadedUsers, err)
	}
	loadedRules, err := s.LoadRules()
	if err != nil || loadedRules[2] != 40 {
		t.Fatalf("rule usage was not durable: %v %v", loadedRules, err)
	}
	loadedRetired, err := s.LoadRetired()
	if err != nil || loadedRetired["1:0"] != retired["1:0"] {
		t.Fatalf("retired usage was not durable: %v %v", loadedRetired, err)
	}
}

func TestStateCheckpointTracksEpochRetirementAndAcknowledgement(t *testing.T) {
	s, err := OpenState(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	old := DiskUser{Metered: true, Issued: 100, Spent: 50, Traffic: 30, Charged: 30}
	if err := s.SaveUser(1, old); err != nil {
		t.Fatal(err)
	}
	if err := s.checkpoint(nil, nil, map[string]DiskUser{"1:0": old}); err != nil {
		t.Fatal(err)
	}
	newer := DiskUser{Metered: true, Epoch: 1, Issued: 200}
	if err := s.SaveUser(1, newer); err != nil {
		t.Fatal(err)
	}
	// SaveUser retires the previous current record, invalidating the mirror.
	old.Traffic, old.Charged = 40, 40
	if err := s.checkpoint(nil, nil, map[string]DiskUser{"1:0": old}); err != nil {
		t.Fatal(err)
	}
	if err := s.DropRetired("1:0"); err != nil {
		t.Fatal(err)
	}
	// If the record is presented again, it must be recreated rather than
	// skipped because it matched the value written before acknowledgement.
	if err := s.checkpoint(nil, nil, map[string]DiskUser{"1:0": old}); err != nil {
		t.Fatal(err)
	}
	retired, err := s.LoadRetired()
	if err != nil || retired["1:0"] != old {
		t.Fatalf("retired record lost: %v %v", retired, err)
	}
}

func TestCheckpointConcurrentTrafficRemainsDurable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := OpenState(path)
	if err != nil {
		t.Fatal(err)
	}
	e, err := NewEngine(s, 100)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Apply(&pb.ControlMessage{Users: []*pb.UserPolicy{{Id: 1, Enabled: true, Issued: 1 << 20}}}); err != nil {
		t.Fatal(err)
	}
	a := e.users[1]
	a.active.Store(1)
	var workers sync.WaitGroup
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for j := 0; j < 500; j++ {
				n, err := a.tryTake(1)
				if err != nil || n != 1 {
					if err == nil {
						err = ErrQuota
					}
					errs <- err
					return
				}
				a.count(n)
			}
		}()
	}
	for i := 0; i < 100; i++ {
		if _, err := e.Report(); err != nil {
			t.Fatal(err)
		}
	}
	workers.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if _, err := e.Report(); err != nil {
		t.Fatal(err)
	}
	e.Close()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenState(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	users, err := s.LoadUsers()
	if err != nil || users[1].Traffic != 2000 || users[1].Charged != 2000 || users[1].Spent < 2000 {
		t.Fatalf("concurrent traffic was not durable: %v %v", users, err)
	}
}
