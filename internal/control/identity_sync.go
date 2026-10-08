package control

import (
	"context"
	"sync"
	"time"
)

func identityRetryDelay(failures int) time.Duration {
	if failures > 6 {
		return time.Hour
	}
	d := time.Minute * time.Duration(1<<max(failures, 0))
	if d > time.Hour {
		return time.Hour
	}
	return d
}

func (s *Store) RunIdentitySync(ctx context.Context) error {
	type result struct {
		id  string
		err error
	}
	type schedule struct {
		next     time.Time
		failures int
		busy     bool
	}
	jobs := make(chan string)
	results := make(chan result, 4)
	workerCtx, cancel := context.WithCancel(ctx)
	var workers sync.WaitGroup
	for i := 0; i < 4; i++ {
		workers.Go(func() {
			for {
				select {
				case <-workerCtx.Done():
					return
				case id := <-jobs:
					err := s.RefreshIdentity(workerCtx, id)
					select {
					case results <- result{id, err}:
					case <-workerCtx.Done():
						return
					}
				}
			}
		})
	}
	defer func() { cancel(); workers.Wait() }()
	states := make(map[string]schedule)
	poll := func() error {
		rows, err := s.db.QueryContext(ctx, `SELECT t.id,coalesce(i.last_success,0) FROM tailnets t JOIN credentials c ON c.tailnet_id=t.id JOIN users u ON u.id=t.owner_id LEFT JOIN identity_snapshots i ON i.tailnet_id=t.id WHERE t.enabled=1 AND u.enabled=1 AND c.status!='missing' ORDER BY t.id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		active := make(map[string]bool)
		for rows.Next() {
			var id string
			var last int64
			if err := rows.Scan(&id, &last); err != nil {
				return err
			}
			active[id] = true
			state, ok := states[id]
			if !ok {
				state.next = time.Unix(last, 0).Add(time.Minute)
			}
			if !state.busy && !s.now().Before(state.next) {
				select {
				case jobs <- id:
					state.busy = true
				default:
				}
			}
			states[id] = state
		}
		if err := rows.Err(); err != nil {
			return err
		}
		rows.Close()
		for id, state := range states {
			if !active[id] && !state.busy {
				delete(states, id)
			}
		}
		return s.refreshExpiryEvents(ctx)
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	if err := poll(); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case r := <-results:
			state := states[r.id]
			state.busy = false
			if r.err != nil {
				state.failures++
			} else {
				state.failures = 0
			}
			state.next = s.now().Add(identityRetryDelay(state.failures))
			states[r.id] = state
		case <-ticker.C:
			if err := poll(); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
		}
	}
}
