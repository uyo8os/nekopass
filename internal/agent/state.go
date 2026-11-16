package agent

import (
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"sync"
	"time"

	"github.com/nekopass/nekopass/internal/control"
	pb "github.com/nekopass/nekopass/internal/protocol"
	bolt "go.etcd.io/bbolt"
	"google.golang.org/protobuf/proto"
)

type State struct {
	db       *bolt.DB
	Instance string
	// These mirrors only contain values committed by this process. An absent
	// entry is treated as dirty, including after opening or restoring state.
	mu      sync.Mutex
	closed  bool
	config  *pb.ControlMessage
	users   map[int64]DiskUser
	rules   map[int64]int64
	retired map[string]DiskUser
}
type DiskUser struct {
	Metered        bool
	Charged        int64
	ChargeFraction int64
	Epoch          int64
	Issued         int64
	Spent          int64
	Released       int64
	Traffic        int64
	UnlimitedSpent int64
}

func OpenState(path string) (*State, error) {
	db, e := bolt.Open(path, 0600, &bolt.Options{Timeout: time.Second})
	if e != nil {
		return nil, e
	}
	s := &State{db: db, users: make(map[int64]DiskUser), rules: make(map[int64]int64), retired: make(map[string]DiskUser)}
	e = db.Update(func(tx *bolt.Tx) error {
		for _, n := range []string{"meta", "users", "rules", "retired_users", "handshake_replay", "handshake_replay_expiry"} {
			if _, e := tx.CreateBucketIfNotExists([]byte(n)); e != nil {
				return e
			}
		}
		b := tx.Bucket([]byte("meta"))
		s.Instance = string(b.Get([]byte("instance")))
		if s.Instance == "" {
			s.Instance = control.Secret()
			if e := b.Put([]byte("restore_pending"), []byte("1")); e != nil {
				return e
			}
			return b.Put([]byte("instance"), []byte(s.Instance))
		}
		if b.Get([]byte("config")) == nil && tx.Bucket([]byte("users")).Stats().KeyN == 0 && tx.Bucket([]byte("rules")).Stats().KeyN == 0 && tx.Bucket([]byte("retired_users")).Stats().KeyN == 0 {
			return b.Put([]byte("restore_pending"), []byte("1"))
		}
		return nil
	})
	if e != nil {
		db.Close()
		return nil, e
	}
	return s, nil
}
func (s *State) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return s.db.Close()
}
func (s *State) Config() (*pb.ControlMessage, error) {
	c := &pb.ControlMessage{}
	e := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("meta")).Get([]byte("config"))
		if b == nil {
			return nil
		}
		return proto.Unmarshal(b, c)
	})
	return c, e
}
func (s *State) SaveConfig(c *pb.ControlMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("state closed")
	}
	if c.StateRestore == nil && s.config != nil && proto.Equal(s.config, c) {
		return nil
	}
	saved := proto.Clone(c).(*pb.ControlMessage)
	saved.StateRestore = nil
	if s.config != nil && proto.Equal(s.config, saved) {
		return nil
	}
	data, e := proto.Marshal(saved)
	if e != nil {
		return e
	}
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("meta"))
		if c.Node != nil || len(c.Users) > 0 || len(c.Rules) > 0 {
			if err := b.Delete([]byte("restore_pending")); err != nil {
				return err
			}
		}
		return b.Put([]byte("config"), data)
	})
	if err == nil {
		s.config = saved
	}
	return err
}
func (s *State) LoadUsers() (map[int64]DiskUser, error) {
	out := map[int64]DiskUser{}
	e := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("users")).ForEach(func(k, v []byte) error {
			id, e := strconv.ParseInt(string(k), 10, 64)
			if e != nil {
				return e
			}
			var d DiskUser
			if e = json.Unmarshal(v, &d); e != nil {
				return e
			}
			if !d.Metered {
				d.Charged = d.Traffic
				d.Metered = true
			}
			if d.Charged < 0 || d.ChargeFraction < 0 || d.ChargeFraction >= 10000 || d.UnlimitedSpent < 0 || d.Traffic < 0 || d.Spent > math.MaxInt64-d.UnlimitedSpent || d.Charged > d.Spent+d.UnlimitedSpent || d.Spent < 0 || d.Released < 0 || d.Issued < d.Spent || d.Issued-d.Spent < d.Released {
				return errors.New("corrupt quota state")
			}
			out[id] = d
			return nil
		})
	})
	return out, e
}
func (s *State) SaveUser(id int64, d DiskUser) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("state closed")
	}
	if old, ok := s.users[id]; ok && old == d {
		return nil
	}
	data, e := json.Marshal(d)
	if e != nil {
		return e
	}
	retiredKey := ""
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("users"))
		key := []byte(strconv.FormatInt(id, 10))
		if old := b.Get(key); old != nil {
			var previous DiskUser
			if e := json.Unmarshal(old, &previous); e != nil {
				return e
			}
			if d.Epoch < previous.Epoch {
				return errors.New("refusing stale current-period state")
			}
			if d.Epoch > previous.Epoch {
				retiredKey = epochKey(id, previous.Epoch)
				if e := tx.Bucket([]byte("retired_users")).Put([]byte(retiredKey), old); e != nil {
					return e
				}
			}
		}
		return b.Put(key, data)
	})
	if err == nil {
		s.users[id] = d
		if retiredKey != "" {
			delete(s.retired, retiredKey)
		}
	}
	return err
}

func epochKey(id, epoch int64) string {
	return strconv.FormatInt(id, 10) + ":" + strconv.FormatInt(epoch, 10)
}
func (s *State) LoadRetired() (map[string]DiskUser, error) {
	out := map[string]DiskUser{}
	e := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("retired_users")).ForEach(func(k, v []byte) error {
			var d DiskUser
			if e := json.Unmarshal(v, &d); e != nil {
				return e
			}
			if !d.Metered {
				d.Charged = d.Traffic
				d.Metered = true
			}
			out[string(k)] = d
			return nil
		})
	})
	return out, e
}
func (s *State) DropRetired(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := s.db.Update(func(tx *bolt.Tx) error { return tx.Bucket([]byte("retired_users")).Delete([]byte(key)) })
	if err == nil {
		delete(s.retired, key)
	}
	return err
}
func (s *State) LoadRules() (map[int64]int64, error) {
	out := map[int64]int64{}
	e := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("rules")).ForEach(func(k, v []byte) error {
			id, e := strconv.ParseInt(string(k), 10, 64)
			if e != nil {
				return e
			}
			n, e := strconv.ParseInt(string(v), 10, 64)
			out[id] = n
			return e
		})
	})
	return out, e
}
func (s *State) SaveRules(values map[int64]int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("state closed")
	}
	dirty := make(map[int64]int64)
	for id, n := range values {
		if previous, ok := s.rules[id]; !ok || previous != n {
			dirty[id] = n
		}
	}
	if len(dirty) == 0 {
		return nil
	}
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("rules"))
		for id, n := range dirty {
			if e := b.Put([]byte(strconv.FormatInt(id, 10)), []byte(strconv.FormatInt(n, 10))); e != nil {
				return e
			}
		}
		return nil
	})
	if err == nil {
		for id, n := range dirty {
			s.rules[id] = n
		}
	}
	return err
}

func (s *State) Checkpoint(users map[int64]DiskUser, rules map[int64]int64) error {
	return s.checkpoint(users, rules, nil)
}
func (s *State) checkpoint(users map[int64]DiskUser, rules map[int64]int64, retired map[string]DiskUser) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("state closed")
	}
	dirtyUsers := make(map[int64]DiskUser)
	dirtyRules := make(map[int64]int64)
	dirtyRetired := make(map[string]DiskUser)
	for id, d := range users {
		if previous, ok := s.users[id]; !ok || previous != d {
			dirtyUsers[id] = d
		}
	}
	for id, n := range rules {
		if previous, ok := s.rules[id]; !ok || previous != n {
			dirtyRules[id] = n
		}
	}
	for key, d := range retired {
		if previous, ok := s.retired[key]; !ok || previous != d {
			dirtyRetired[key] = d
		}
	}
	if len(dirtyUsers) == 0 && len(dirtyRules) == 0 && len(dirtyRetired) == 0 {
		return nil
	}
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("users"))
		for id, d := range dirtyUsers {
			data, e := json.Marshal(d)
			if e != nil {
				return e
			}
			if e = b.Put([]byte(strconv.FormatInt(id, 10)), data); e != nil {
				return e
			}
		}
		b = tx.Bucket([]byte("rules"))
		for id, n := range dirtyRules {
			if e := b.Put([]byte(strconv.FormatInt(id, 10)), []byte(strconv.FormatInt(n, 10))); e != nil {
				return e
			}
		}
		b = tx.Bucket([]byte("retired_users"))
		for key, d := range dirtyRetired {
			data, e := json.Marshal(d)
			if e != nil {
				return e
			}
			if e = b.Put([]byte(key), data); e != nil {
				return e
			}
		}
		return nil
	})
	if err == nil {
		for id, d := range dirtyUsers {
			s.users[id] = d
		}
		for id, n := range dirtyRules {
			s.rules[id] = n
		}
		for key, d := range dirtyRetired {
			s.retired[key] = d
		}
	}
	return err
}
