package store

import (
	"fmt"
	"sync"
	"testing"
)

// TestConfigSnapshotIsImmutable pins the copy-on-write contract: Config()
// hands out the live pointer's *current* struct, and Update swaps in a fresh
// copy — so a snapshot must never be written through. Before the fix admin
// handlers mutated the returned struct directly while /v1 traffic iterated
// the same maps: Go treats concurrent map iteration + map write as a fatal,
// uncatchable error that kills the process.
func TestConfigSnapshotIsImmutable(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ZEN_GATE_HOME", dir)
	s, err := Open()
	if err != nil {
		t.Fatal(err)
	}

	// Writing through a snapshot must not reach the store…
	snap := s.Config()
	snap.WebUIPassword = "hacked"
	snap.AgentKeys["ghost"] = "x"
	snap.ShareKeys["ghost"] = "y"

	if got := s.Config(); got.WebUIPassword == "hacked" ||
		got.AgentKeys["ghost"] == "x" || got.ShareKeys["ghost"] == "y" {
		t.Fatal("a Config() snapshot leaked writes back into the store")
	}

	// …while Update is the one sanctioned write path.
	s.Update(func(c *Config) { c.WebUIPassword = "real" })
	if got := s.Config(); got.WebUIPassword != "real" {
		t.Fatalf("Update did not persist: %q", got.WebUIPassword)
	}
	// The pre-update snapshot stays coherent (old value, no ghost keys).
	if snap.WebUIPassword == "real" {
		t.Fatal("Update mutated a snapshot handed out earlier")
	}
}

// TestUpdateConcurrentWithConfig is the race-detector guard for the COW
// refactor: readers take snapshots and (incorrectly, on purpose) mutate them
// while writers apply updates. With the live-config writes this combination
// fatal-crashed the process; with COW it must run clean. Run with -race.
func TestUpdateConcurrentWithConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ZEN_GATE_HOME", dir)
	s, err := Open()
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	// writers: rotate the password and share keys through Update
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; ; j++ {
				select {
				case <-stop:
					return
				default:
				}
				s.Update(func(c *Config) {
					c.WebUIPassword = fmt.Sprintf("pw-%d-%d", id, j)
					c.ShareKeys[fmt.Sprintf("k%d", j%8)] = fmt.Sprintf("v-%d-%d", id, j)
				})
			}
		}(i)
	}
	// readers: snapshot, then poke at the copy the way the old handlers did
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; ; j++ {
				select {
				case <-stop:
					return
				default:
				}
				c := s.Config()
				c.WebUIPassword = "reader-write"
				c.AgentKeys[fmt.Sprintf("a%d", j%8)] = "reader"
				c.ShareKeys[fmt.Sprintf("k%d", j%8)] = "reader"
				_ = len(c.AgentKeys) + len(c.ShareKeys)
			}
		}(i)
	}
	// key helpers go through their own lock on top of Update
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; ; j++ {
				select {
				case <-stop:
					return
				default:
				}
				_ = s.KeyForAgent(fmt.Sprintf("agent-%d-%d", id, j%4))
				_ = s.CreateShareKey(fmt.Sprintf("label-%d", j%4))
			}
		}(i)
	}

	// let the three groups overlap, then wind down
	for i := 0; i < 20000; i++ {
		_ = s.Config().WebUIPassword
	}
	close(stop)
	wg.Wait()

	// The store must still be coherent and persistable afterwards.
	if err := s.Save(); err != nil {
		t.Fatalf("save after concurrent updates: %v", err)
	}
	reopened, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Config().WebUIPassword == "reader-write" {
		t.Fatal("a reader's snapshot write was persisted — COW contract broken")
	}
}
