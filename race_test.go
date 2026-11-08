package ledgercore

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Concurrent writers. SQLite takes the write lock when a write transaction begins, so
// "read, check, then write" inside one transaction cannot interleave with another
// writer. PostgreSQL reads committed data by default, so two writers can both pass a
// check. These tests race writers against the rules that depend on a check, and hold
// both backends to the same answer. Each rule is raced for several rounds, because a
// race that fails one run in ten is still a failure.

const raceRounds, raceWriters = 12, 8

// race runs fn on n goroutines released together, and returns their results by index.
func race[T any](n int, fn func(i int) T) []T {
	out := make([]T, n)
	var ready, done sync.WaitGroup
	start := make(chan struct{})
	ready.Add(n)
	done.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer done.Done()
			ready.Done()
			<-start
			out[i] = fn(i)
		}(i)
	}
	ready.Wait()
	close(start)
	done.Wait()
	return out
}

func raceDB(t *testing.T, actor Actor) (*DB, *Project) {
	t.Helper()
	db := openAs(t, filepath.Join(t.TempDir(), "race.db"), actor, nil)
	return db, mustProject(t, db, "race")
}

// Two editors holding the same version: exactly one save wins, the rest are told they
// are out of date. Without this, the second silently overwrites the first.
func TestRace_editConflictCheckIsAtomic(t *testing.T) {
	db, p := raceDB(t, alice)
	it := mustItem(t, db, p.ID, "t")
	for round := 0; round < raceRounds; round++ {
		cur, _ := db.GetItem(ctx, it.ID)
		res := race(raceWriters, func(i int) error {
			title := fmt.Sprintf("round %d writer %d", round, i)
			_, err := db.UpdateItemFields(ctx, it.ID, ItemUpdate{Fields: ItemFields{Title: &title}, ExpectedUpdatedAt: cur.UpdatedAt})
			return err
		})
		won, conflicts := 0, 0
		for _, err := range res {
			switch {
			case err == nil:
				won++
			case errors.Is(err, ErrConflict):
				conflicts++
			default:
				t.Fatalf("round %d: unexpected error: %v", round, err)
			}
		}
		if won != 1 || conflicts != raceWriters-1 {
			t.Fatalf("round %d: %d saves won and %d were refused; want exactly 1 and %d (a lost update)", round, won, conflicts, raceWriters-1)
		}
	}
}

// A project that has an owner keeps one, even when its two owners are removed or
// demoted at the same instant, each seeing the other still there.
func TestRace_lastOwnerSurvives(t *testing.T) {
	db, p := raceDB(t, alice)
	for round := 0; round < raceRounds; round++ {
		a, b := fmt.Sprintf("person:a%d", round), fmt.Sprintf("person:b%d", round)
		for _, m := range []string{a, b} {
			if _, _, err := db.AddProjectMember(ctx, p.ID, m, "owner"); err != nil {
				t.Fatal(err)
			}
		}
		// Make the pair the only owners: alice (the creator) steps down first.
		if _, err := db.SetProjectMemberRole(ctx, p.ID, "person:alice", "member"); err != nil && !errors.Is(err, ErrLastOwner) {
			t.Fatal(err)
		}
		race(4, func(i int) error {
			switch i {
			case 0:
				_, err := db.RemoveProjectMember(ctx, p.ID, a)
				return err
			case 1:
				_, err := db.RemoveProjectMember(ctx, p.ID, b)
				return err
			case 2:
				_, err := db.SetProjectMemberRole(ctx, p.ID, a, "viewer")
				return err
			default:
				_, err := db.SetProjectMemberRole(ctx, p.ID, b, "viewer")
				return err
			}
		})
		var owners int
		db.conn.QueryRow(`SELECT COUNT(*) FROM project_members WHERE project_id = ? AND role = 'owner' AND deleted_at IS NULL`, p.ID).Scan(&owners)
		if owners < 1 {
			t.Fatalf("round %d: the project was left with no owner", round)
		}
		// Reset for the next round: owners back to exactly alice.
		db.conn.Exec(`DELETE FROM project_members WHERE project_id = ?`, p.ID)
		db.conn.Exec(`INSERT INTO project_members (id, project_id, member, role, created_at) VALUES (?, ?, 'person:alice', 'owner', ?)`,
			fmt.Sprintf("reset-%d", round), p.ID, nowUTC())
	}
}

// The generic soft delete must not be a way round it either.
func TestRace_lastOwnerSurvivesTheGenericDelete(t *testing.T) {
	db, p := raceDB(t, alice)
	for round := 0; round < raceRounds; round++ {
		if _, _, err := db.AddProjectMember(ctx, p.ID, "person:bob", "owner"); err != nil {
			t.Fatal(err)
		}
		ms, _ := db.ListProjectMembers(ctx, p.ID)
		race(len(ms), func(i int) error { return db.SoftDelete(ctx, "project_member", ms[i].ID) })
		var owners int
		db.conn.QueryRow(`SELECT COUNT(*) FROM project_members WHERE project_id = ? AND role = 'owner' AND deleted_at IS NULL`, p.ID).Scan(&owners)
		if owners < 1 {
			t.Fatalf("round %d: both owner rows were deleted at once", round)
		}
		db.conn.Exec(`DELETE FROM project_members WHERE project_id = ?`, p.ID)
		db.conn.Exec(`INSERT INTO project_members (id, project_id, member, role, created_at) VALUES (?, ?, 'person:alice', 'owner', ?)`,
			fmt.Sprintf("reset-%d", round), p.ID, nowUTC())
	}
}

// Start and stop pair up: one running timer per item, and a stop needs a start.
func TestRace_timerPairing(t *testing.T) {
	db, p := raceDB(t, alice)
	it := mustItem(t, db, p.ID, "t")
	for round := 0; round < raceRounds; round++ {
		starts := race(raceWriters, func(int) error { _, err := db.StartTimer(ctx, it.ID); return err })
		if n := countNil(starts); n != 1 {
			t.Fatalf("round %d: %d timers started at once; want exactly 1", round, n)
		}
		stops := race(raceWriters, func(int) error { _, err := db.StopTimer(ctx, it.ID); return err })
		if n := countNil(stops); n != 1 {
			t.Fatalf("round %d: %d timers stopped at once; want exactly 1", round, n)
		}
	}
}

// The same label, assignee, watcher or member added by several writers at once: one is
// added, the others are told it was already there, and nobody gets an error.
func TestRace_addingTheSameThingTwiceIsIdempotent(t *testing.T) {
	db, p := raceDB(t, alice)
	it := mustItem(t, db, p.ID, "t")
	adds := map[string]func(i int) (bool, error){
		"label": func(int) (bool, error) { _, added, err := db.AddLabel(ctx, it.ID, "same"); return added, err },
		"assignee": func(int) (bool, error) {
			_, added, err := db.AddAssignee(ctx, it.ID, "person:bob")
			return added, err
		},
		"watcher": func(int) (bool, error) {
			_, added, err := db.AddWatcher(ctx, it.ID, "person:bob")
			return added, err
		},
		"project watcher": func(int) (bool, error) {
			_, added, err := db.AddProjectWatcher(ctx, p.ID, "person:bob")
			return added, err
		},
		"project member": func(int) (bool, error) {
			_, added, err := db.AddProjectMember(ctx, p.ID, "person:bob", "member")
			return added, err
		},
	}
	for name, add := range adds {
		type r struct {
			added bool
			err   error
		}
		res := race(raceWriters, func(i int) r { a, err := add(i); return r{a, err} })
		added := 0
		for _, x := range res {
			if x.err != nil {
				t.Errorf("%s: a concurrent add failed: %v", name, x.err)
			}
			if x.added {
				added++
			}
		}
		if added != 1 {
			t.Errorf("%s: %d writers each believe they added it; want exactly 1", name, added)
		}
	}
}

// Several programs writing to a project key that does not exist yet must all end up
// in the same project, not one project each or an error for the losers.
func TestRace_theSameNewProjectKeyIsOneProject(t *testing.T) {
	db := openAs(t, filepath.Join(t.TempDir(), "race.db"), alice, nil)
	for round := 0; round < raceRounds; round++ {
		key := fmt.Sprintf("fresh-%d", round)
		type r struct {
			id  string
			err error
		}
		res := race(raceWriters, func(int) r {
			p, err := db.GetOrCreateProjectForWrite(ctx, key, key)
			if err != nil {
				return r{err: err}
			}
			return r{id: p.ID}
		})
		ids := map[string]bool{}
		for _, x := range res {
			if x.err != nil {
				t.Fatalf("round %d: a concurrent create failed: %v", round, x.err)
			}
			ids[x.id] = true
		}
		var rows int
		db.conn.QueryRow(`SELECT COUNT(*) FROM projects WHERE key = ?`, key).Scan(&rows)
		if len(ids) != 1 || rows != 1 {
			t.Fatalf("round %d: %d distinct projects returned, %d rows; want 1 and 1", round, len(ids), rows)
		}
	}
}

// Two people relating the same pair of tickets in opposite directions must not
// deadlock one of them.
func TestRace_relatingInOppositeDirectionsDoesNotDeadlock(t *testing.T) {
	db, p := raceDB(t, alice)
	a, b := mustItem(t, db, p.ID, "a"), mustItem(t, db, p.ID, "b")
	for round := 0; round < raceRounds; round++ {
		res := race(raceWriters, func(i int) error {
			from, to := a.ID, b.ID
			if i%2 == 1 {
				from, to = b.ID, a.ID
			}
			_, err := db.RelateItems(ctx, from, to, "related_to")
			return err
		})
		for _, err := range res {
			if err != nil && strings.Contains(strings.ToLower(err.Error()), "deadlock") {
				t.Fatalf("round %d: %v", round, err)
			}
			if err != nil {
				t.Fatalf("round %d: %v", round, err)
			}
		}
	}
}

func countNil(errs []error) int {
	n := 0
	for _, e := range errs {
		if e == nil {
			n++
		}
	}
	return n
}
