// Package releases manages terraform releases.
package releases

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/tofutf/tofutf/internal"
	"github.com/tofutf/tofutf/internal/semver"
	"github.com/tofutf/tofutf/internal/sql"
)

const (
	DefaultTerraformVersion = "1.6.0"
	LatestVersionString     = "latest"
)

type (
	Service struct {
		logger *slog.Logger
		*downloader
		latestChecker

		db *db
	}

	Options struct {
		*sql.Pool

		Logger          *slog.Logger
		TerraformBinDir string // destination directory for terraform binaries
	}
)

func NewService(opts Options) *Service {
	svc := &Service{
		logger:        opts.Logger,
		db:            &db{opts.Pool},
		latestChecker: latestChecker{latestEndpoint},
		downloader:    NewDownloader(opts.TerraformBinDir),
	}
	return svc
}

// StartLatestChecker starts the latest checker go routine, checking the Hashicorp
// API endpoint for a new latest version.
func (s *Service) StartLatestChecker(ctx context.Context) {
	check := func() {
		err := func() error {
			before, checkpoint, err := s.GetLatest(ctx)
			if err != nil {
				return err
			}
			after, err := s.latestChecker.check(checkpoint)
			if err != nil {
				return err
			}
			if after == "" {
				// check was skipped (too early)
				return nil
			}
			// perform sanity check
			if n := semver.Compare(after, before); n < 0 {
				return fmt.Errorf("endpoint returned older version: before: %s; after: %s", before, after)
			}
			// update db (even if version hasn't changed we need to update the
			// checkpoint)
			if err := s.db.updateLatestVersion(ctx, after); err != nil {
				return err
			}

			s.logger.Debug("checked latest terraform version", "before", before, "after", after)
			return nil
		}()
		if err != nil {
			s.logger.Error("checking latest terraform version", "err", err)
		}
	}
	// check once at startup
	check()
	// ...and check every 5 mins thereafter
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		for {
			select {
			case <-ticker.C:
				check()
			case <-ctx.Done():
				ticker.Stop()
				return
			}
		}
	}()
}

// GetLatest returns the latest terraform version and the time when it was
// fetched; if it has not yet been fetched then the default version is returned
// instead along with zero time.
func (s *Service) GetLatest(ctx context.Context) (string, time.Time, error) {
	latest, checkpoint, err := s.db.getLatest(ctx)
	if errors.Is(err, internal.ErrResourceNotFound) {
		// no latest version has yet been persisted to the database so return
		// the default version instead
		return DefaultTerraformVersion, time.Time{}, nil
	} else if err != nil {
		return "", time.Time{}, err
	}
	return latest, checkpoint, nil
}
