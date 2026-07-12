package daemon

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/ehsan200/em-xray/core/xray"
)

// SubFetcher schedules and executes subscription refreshes. A successful refresh
// swaps the volatile node pool (ReplaceNodes) and fires onChange, which the
// supervisor wires to SyncDialerMembers for a zero-restart live update.
type SubFetcher struct {
	store    *xray.Store
	client   *http.Client
	log      *log.Logger
	onChange func() // nil-safe; invoked after a successful refresh
}

// NewSubFetcher builds a fetcher with a direct HTTP client. Censored-URL
// fallback through existing outbounds is layered on in a later phase.
func NewSubFetcher(store *xray.Store, logger *log.Logger, onChange func()) *SubFetcher {
	return &SubFetcher{
		store:    store,
		client:   &http.Client{Timeout: 30 * time.Second},
		log:      logger,
		onChange: onChange,
	}
}

// Start runs the scheduler loop until ctx is cancelled: an immediate due-pass,
// then one every SubSchedTick.
func (f *SubFetcher) Start(ctx context.Context) {
	go func() {
		t := time.NewTicker(xray.SubSchedTick)
		defer t.Stop()
		f.RefreshDue(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				f.RefreshDue(ctx)
			}
		}
	}()
}

// RefreshDue refreshes every subscription that IsDue right now.
func (f *SubFetcher) RefreshDue(ctx context.Context) {
	subs, err := f.store.ListSubscriptions()
	if err != nil {
		f.log.Printf("sub: list failed: %v", err)
		return
	}
	now := time.Now()
	changed := false
	for i := range subs {
		if !xray.IsDue(subs[i], now) {
			continue
		}
		if n, err := f.refresh(ctx, &subs[i]); err != nil {
			f.log.Printf("sub %q: refresh failed: %v", subs[i].Name, err)
		} else {
			f.log.Printf("sub %q: %d nodes", subs[i].Name, n)
			changed = true
		}
	}
	if changed {
		f.fireChange()
	}
}

// RefreshAll force-refreshes every enabled subscription (manual refresh-all).
func (f *SubFetcher) RefreshAll(ctx context.Context) error {
	subs, err := f.store.ListSubscriptions()
	if err != nil {
		return err
	}
	changed := false
	for i := range subs {
		if !subs[i].Enabled {
			continue
		}
		if _, err := f.refresh(ctx, &subs[i]); err != nil {
			f.log.Printf("sub %q: refresh failed: %v", subs[i].Name, err)
			continue
		}
		changed = true
	}
	if changed {
		f.fireChange()
	}
	return nil
}

// RefreshOne force-refreshes a single subscription by id (manual refresh-one).
func (f *SubFetcher) RefreshOne(ctx context.Context, id uint) (int, error) {
	sub, err := f.store.GetSubscription(id)
	if err != nil {
		return 0, err
	}
	n, err := f.refresh(ctx, sub)
	if err != nil {
		return 0, err
	}
	f.fireChange()
	return n, nil
}

// refresh does the fetch → parse → ReplaceNodes for one subscription and records
// the outcome (LastFetched/LastError, and quota when the header was present).
// Returns the node count on success.
func (f *SubFetcher) refresh(ctx context.Context, sub *xray.Subscription) (int, error) {
	body, quota, err := xray.FetchSubscriptionBody(ctx, f.client, sub.URL, sub.UserAgent)
	if err != nil {
		f.record(sub, quota, err)
		return 0, err
	}
	nodes, err := xray.ParseSubscriptionBody(sub.Name, body)
	if err != nil {
		f.record(sub, quota, err)
		return 0, err
	}
	if err := f.store.ReplaceNodes(sub.ID, nodes); err != nil {
		f.record(sub, quota, err)
		return 0, err
	}
	f.record(sub, quota, nil)
	return len(nodes), nil
}

// record persists fetch outcome + quota. Quota is only applied when present, so
// a header-less response keeps the previously-stored figures.
func (f *SubFetcher) record(sub *xray.Subscription, quota xray.Quota, ferr error) {
	sub.LastFetched = time.Now()
	if ferr != nil {
		sub.LastError = ferr.Error()
	} else {
		sub.LastError = ""
	}
	if quota.HasData {
		sub.Upload, sub.Download, sub.Total, sub.Expire = quota.Upload, quota.Download, quota.Total, quota.Expire
	}
	if err := f.store.UpdateSubscription(sub); err != nil {
		f.log.Printf("sub %q: persist failed: %v", sub.Name, err)
	}
}

func (f *SubFetcher) fireChange() {
	if f.onChange != nil {
		f.onChange()
	}
}
