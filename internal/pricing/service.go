package pricing

import (
	"context"
	"errors"

	"billing-backend/internal/domain"
	"billing-backend/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Service struct{ DB *pgxpool.Pool }

func (s *Service) Current(ctx context.Context, key string) (domain.PriceSnapshot, error) {
	return s.CurrentIn(ctx, s.DB, key)
}

func (s *Service) CurrentIn(ctx context.Context, q store.Querier, key string) (domain.PriceSnapshot, error) {
	var price domain.PriceSnapshot
	err := q.QueryRow(ctx, `SELECT hourly_usd_nano,rate_unit,verified,version,observed_at FROM dstack_instance_prices WHERE price_key=$1`, key).Scan(&price.HourlyUSD, &price.RateUnit, &price.Verified, &price.Version, &price.ObservedAt)
	return price, err
}

func (s *Service) Set(ctx context.Context, key string, price domain.USDNano, verified bool, version string, verifiedBy string) error {
	if key == "" || price <= 0 || version == "" || len(version) > 200 {
		return errors.New("price, version, and verification state are required")
	}
	var actor any
	if verified {
		actor = verifiedBy
	}
	_, err := s.DB.Exec(ctx, `INSERT INTO dstack_instance_prices(price_key,hourly_usd_nano,rate_unit,verified,version,verified_by) VALUES($1,$2,'instance_hour',$3,$4,$5) ON CONFLICT(price_key) DO UPDATE SET hourly_usd_nano=EXCLUDED.hourly_usd_nano,verified=EXCLUDED.verified,version=EXCLUDED.version,verified_by=EXCLUDED.verified_by,observed_at=now()`, key, int64(price), verified, version, actor)
	return err
}
