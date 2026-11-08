package compute

import (
	"context"
	"testing"
	"time"

	"billing-backend/internal/domain"
)

type observationSourceFunc func(context.Context, domain.TenantID, string, *time.Time, string, time.Time) (ObservationPage, error)

func (f observationSourceFunc) ListTenantRuns(ctx context.Context, tenant domain.TenantID, at *time.Time, id string, now time.Time) (ObservationPage, error) {
	return f(ctx, tenant, "runs", at, id, now)
}

func (f observationSourceFunc) ListTenantInstances(ctx context.Context, tenant domain.TenantID, at *time.Time, id string, now time.Time) (ObservationPage, error) {
	return f(ctx, tenant, "instances", at, id, now)
}

func (f observationSourceFunc) ListTenantEvents(ctx context.Context, tenant domain.TenantID, at *time.Time, id string, now time.Time) (ObservationPage, error) {
	return f(ctx, tenant, "events", at, id, now)
}

func TestObservationSourceReceivesTenantAndOverlapCursor(t *testing.T) {
	cutoff := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	called := false
	source := observationSourceFunc(func(_ context.Context, tenant domain.TenantID, stream string, at *time.Time, id string, now time.Time) (ObservationPage, error) {
		called = true
		if tenant != "tenant-1" || stream != "events" || at == nil || !at.Equal(cutoff) || id != "event-9" || !now.After(cutoff) {
			t.Fatalf("poll args = %q %q %v %q %v", tenant, stream, at, id, now)
		}
		return ObservationPage{Done: true}, nil
	})
	page, err := source.ListTenantEvents(context.Background(), "tenant-1", &cutoff, "event-9", cutoff.Add(time.Minute))
	if err != nil || !called || !page.Done {
		t.Fatalf("page=%+v called=%v err=%v", page, called, err)
	}
}
