//go:build !laptop

package store

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"sync/atomic"
	"time"

	"cloud.google.com/go/firestore"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fireStore keeps the store in Cloud Firestore. Documents hold the store's types as they are, so fields have their
// Go names. Pending messages, codes and events also get an ExpireAt field: a TTL policy on it lets Firestore delete
// what nothing will read again, but nothing here depends on one.
type fireStore struct {
	c     *firestore.Client
	added atomic.Int64 // events this process added, to prune the log every pruneEvery of them
}

// A pending message's ExpireAt comes a week after it expires, long after TakeExpired should have told its sender.
const forgetPending = 7 * 24 * time.Hour

// takeAtOnce is how many expired messages one transaction takes, to stay well under Firestore's commit size.
const takeAtOnce = 200

type pendingDoc struct {
	Pending
	ExpireAt time.Time
}

type codeDoc struct {
	Code
	ExpireAt time.Time
}

type eventDoc struct {
	Event
	ExpireAt time.Time
}

type kvDoc struct{ Value []byte }

func openFirestore(ctx context.Context, project, database string) (Store, error) {
	if project == "" {
		return nil, errors.New("firestore:// needs a project, such as firestore://my-project")
	}
	if database == "" {
		database = firestore.DefaultDatabaseID
	}
	c, err := firestore.NewClientWithDatabase(ctx, project, database)
	if err != nil {
		return nil, err
	}
	return &fireStore{c: c}, nil
}

// doc is the document for id in a collection. The id is escaped: a slash in it would make it a path.
func (s *fireStore) doc(collection, id string) *firestore.DocumentRef {
	return s.c.Collection(collection).Doc(url.PathEscape(id))
}

// all reads every document an iterator returns into a T.
func all[T any](it *firestore.DocumentIterator) ([]T, error) {
	docs, err := it.GetAll()
	if err != nil {
		return nil, err
	}
	out := make([]T, len(docs))
	for i, d := range docs {
		if err := d.DataTo(&out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *fireStore) Users(ctx context.Context) ([]User, error) {
	return all[User](s.c.Collection("users").Documents(ctx))
}

func (s *fireStore) PutUser(ctx context.Context, u User) error {
	_, err := s.doc("users", u.Email).Set(ctx, u)
	return err
}

func (s *fireStore) Devices(ctx context.Context) ([]Device, error) {
	return all[Device](s.c.Collection("devices").Documents(ctx))
}

func (s *fireStore) PutDevice(ctx context.Context, d Device) error {
	_, err := s.doc("devices", d.ID).Set(ctx, d)
	return err
}

func (s *fireStore) SeenDevice(ctx context.Context, id, version string, at time.Time) error {
	return s.changeDevice(ctx, id, []firestore.Update{{Path: "Version", Value: version}, {Path: "LastSeen", Value: at}})
}

func (s *fireStore) ReportDevice(ctx context.Context, id string, report []byte, at time.Time) error {
	return s.changeDevice(ctx, id, []firestore.Update{{Path: "Report", Value: report}, {Path: "ReportAt", Value: at}})
}

func (s *fireStore) changeDevice(ctx context.Context, id string, u []firestore.Update) error {
	_, err := s.doc("devices", id).Update(ctx, u)
	if status.Code(err) == codes.NotFound {
		return ErrNotFound
	}
	return err
}

func (s *fireStore) AddPending(ctx context.Context, p Pending) error {
	_, err := s.doc("pending", p.Msg.ID).Set(ctx, pendingDoc{p, p.Expires.Add(forgetPending)})
	return err
}

// Pending filters and sorts in Go: a query on Email alone needs no composite index, and a person has few messages.
func (s *fireStore) Pending(ctx context.Context, email string) ([]Pending, error) {
	ps, err := all[Pending](s.c.Collection("pending").Where("Email", "==", email).Documents(ctx))
	if err != nil {
		return nil, err
	}
	now := time.Now()
	ps = slices.DeleteFunc(ps, func(p Pending) bool { return !now.Before(p.Expires) })
	slices.SortFunc(ps, func(a, b Pending) int { return a.Created.Compare(b.Created) })
	return ps, nil
}

func (s *fireStore) DropPending(ctx context.Context, id string) error {
	_, err := s.doc("pending", id).Delete(ctx)
	return err
}

// TakeExpired returns what it took even when a later transaction fails: that much is gone from the store.
func (s *fireStore) TakeExpired(ctx context.Context, now time.Time) ([]Pending, error) {
	var out []Pending
	for {
		var took []Pending
		err := s.c.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
			docs, err := tx.Documents(s.c.Collection("pending").Where("Expires", "<=", now).Limit(takeAtOnce)).GetAll()
			if err != nil {
				return err
			}
			took = make([]Pending, len(docs))
			for i, d := range docs {
				if err := d.DataTo(&took[i]); err != nil {
					return err
				}
				if err := tx.Delete(d.Ref); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return out, err
		}
		out = append(out, took...)
		if len(took) < takeAtOnce {
			return out, nil
		}
	}
}

func (s *fireStore) PutCode(ctx context.Context, c Code) error {
	_, err := s.doc("codes", c.ID).Set(ctx, codeDoc{c, c.Expires})
	return err
}

func (s *fireStore) TakeCode(ctx context.Context, id string) (Code, error) {
	ref := s.doc("codes", id)
	var c Code
	err := s.c.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		d, err := tx.Get(ref)
		if status.Code(err) == codes.NotFound {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if err := d.DataTo(&c); err != nil {
			return err
		}
		return tx.Delete(ref)
	})
	if err != nil {
		return Code{}, err
	}
	if !time.Now().Before(c.Expires) {
		return Code{}, ErrNotFound
	}
	return c, nil
}

func (s *fireStore) Get(ctx context.Context, key string) ([]byte, error) {
	d, err := s.doc("kv", key).Get(ctx)
	if status.Code(err) == codes.NotFound {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var v kvDoc
	err = d.DataTo(&v)
	return v.Value, err
}

func (s *fireStore) Put(ctx context.Context, key string, value []byte) error {
	_, err := s.doc("kv", key).Set(ctx, kvDoc{value})
	return err
}

func (s *fireStore) AddEvent(ctx context.Context, e Event) error {
	_, _, err := s.c.Collection("events").Add(ctx, eventDoc{e, e.At.Add(keepEventsFor)})
	if err != nil || s.added.Add(1)%pruneEvery != 1 {
		return err
	}
	if err := s.pruneEvents(ctx, time.Now()); err != nil {
		return fmt.Errorf("event added, but pruning the log failed: %w", err)
	}
	return nil
}

// pruneEvents deletes the events older than keepEventsFor and those beyond the newest keepEvents. The two queries
// don't overlap, since BulkWriter refuses to delete a document twice.
func (s *fireStore) pruneEvents(ctx context.Context, now time.Time) error {
	events := s.c.Collection("events")
	before := now.Add(-keepEventsFor)
	old, err := events.Where("At", "<", before).Select().Documents(ctx).GetAll()
	if err != nil {
		return err
	}
	extra, err := events.Where("At", ">=", before).OrderBy("At", firestore.Desc).Offset(keepEvents).Select().Documents(ctx).GetAll()
	if err != nil {
		return err
	}
	bw := s.c.BulkWriter(ctx)
	var jobs []*firestore.BulkWriterJob
	for _, d := range append(old, extra...) {
		j, err := bw.Delete(d.Ref)
		if err != nil {
			bw.End()
			return err
		}
		jobs = append(jobs, j)
	}
	bw.End()
	for _, j := range jobs {
		if _, err := j.Results(); err != nil {
			return err
		}
	}
	return nil
}

func (s *fireStore) Events(ctx context.Context, limit int) ([]Event, error) {
	if limit <= 0 {
		return nil, nil
	}
	return all[Event](s.c.Collection("events").OrderBy("At", firestore.Desc).Limit(limit).Documents(ctx))
}

func (s *fireStore) Lease(ctx context.Context, name, holder string, ttl time.Duration) (bool, error) {
	ref := s.doc("leases", name)
	var held bool
	err := s.c.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		var l lease
		d, err := tx.Get(ref)
		if err == nil {
			err = d.DataTo(&l)
		}
		if err != nil && status.Code(err) != codes.NotFound {
			return err
		}
		now := time.Now()
		held = l.Holder == holder || !now.Before(l.Until)
		if !held {
			return nil
		}
		return tx.Set(ref, lease{Holder: holder, Until: now.Add(ttl)})
	})
	if err != nil {
		return false, err
	}
	return held, nil
}

// MaxValue is what a value can take of a document's 1 MiB, its name and field's included.
func (s *fireStore) MaxValue() int { return 1_000_000 }

func (s *fireStore) Close() error { return s.c.Close() }
